package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
)

// activeCancel wraps a context.CancelFunc with a unique pointer identity.
// The pointer is used for compare-and-delete in the dispatch completion path:
// when a finishing run's deferred cleanup fires, it must only remove its own
// entry — not a newer run's entry that was installed in the window between
// the explicit Del and the function return.
type activeCancel struct {
	cancel context.CancelFunc
}

// dispatchOutcome is what dispatchDecision decided for one call, under
// call.SessionID's per-session dispatch mutex. active is true only for the
// "idle: become the active run" branch, in which case genCtx/cancel/ac are
// the run context, its cancel func, and the activeRequests entry this turn
// must release on exit. For the other two branches (cancel-on-entry,
// enqueued) the decision has already been fully realized — a canceled turn
// persisted, or the call queued — and steer/result/err are runTurn's return
// values as-is.
type dispatchOutcome struct {
	steer  SteerOutcome
	active bool
	genCtx context.Context
	cancel context.CancelFunc
	ac     *activeCancel
	result *fantasy.AgentResult
	err    error
}

// dispatchDecision makes the atomic cancel-on-entry / enqueue / become-active
// decision for call, under call.SessionID's per-session dispatch mutex, and
// carries out every side effect that decision implies (closing the accept
// reservation, persisting a canceled turn, enqueueing, or registering the
// active run) before releasing the lock. See sessionAgent.runTurn for how the
// result is used.
//
// Serializing the decision against a concurrent Cancel this way — Cancel
// takes the same per-session lock — guarantees every cancel observes at
// least one of: a cancel mark, an activeRequests entry, or a messageQueue
// entry it then clears. Holding the lock across the busy check and the
// active registration also makes them atomic, so two concurrent in-process
// callers — a burst of channel events, or a channel event racing a typed
// prompt — cannot both pass the busy check and start two runs on the same
// session.
func (a *sessionAgent) dispatchDecision(ctx context.Context, call SessionAgentCall) dispatchOutcome {
	// call.acceptSeq is deliberately not stamped here: this method
	// receives call by value, and dispatchOutcome never hands the
	// mutated copy back, so a write here would be silently discarded by
	// runTurn's own call variable anyway. It also would not help: this
	// call's original accept sequence, from whenever it was first
	// dispatched, is stale by the time a post-summary continuation
	// might need one - see requeueContinuation's own comment for why a
	// continuation mints a fresh sequence instead of reusing this one.
	//
	// dispatched reports the branch taken to call.OnDispatch, at most once
	// and from under the per-session dispatch mutex that took it - see
	// SessionAgentCall.OnDispatch for why the timing is the point.
	var dispatchOnce sync.Once
	dispatched := func(outcome SteerOutcome) {
		if call.OnDispatch == nil {
			return
		}
		dispatchOnce.Do(func() { call.OnDispatch(outcome) })
	}

	s, release := a.session(call.SessionID)
	defer release()
	s.mu.Lock()

	if call.Accepted != nil && a.canceledBySeq(call.SessionID, call.Accepted.seq) {
		// Cancel-on-entry: a cancel arrived while this accepted run was
		// dispatched but not yet active, and this handle's accept sequence
		// is at or below the session's cancel mark. The mark is left in
		// place so sibling handles it also covers observe the same cancel;
		// release the accept reservation, drop the lock, and persist a
		// canceled turn without entering Stream.
		//
		// This path returns before runTurn's streaming defer that publishes
		// RunComplete is installed, so emit the terminal event explicitly.
		// Without it, a caller waiting on RunComplete for this RunID (e.g.
		// `sennit run`, which ignores message events and blocks on
		// RunComplete) would hang on an immediately-canceled accepted run.
		call.Accepted.Close()
		dispatched(SteerCanceled)
		s.mu.Unlock()
		reporter := newCompletionReporter(a, call)
		complete := notify.RunComplete{
			SessionID: call.SessionID,
			RunID:     call.RunID,
			Cancelled: true,
		}
		if err := a.persistCanceledTurn(ctx, call, false); err != nil {
			complete.Error = err.Error()
			reporter.publish(ctx, complete)
			return dispatchOutcome{steer: SteerCanceled, err: err}
		}
		reporter.publish(ctx, complete)
		return dispatchOutcome{steer: SteerCanceled}
	}

	if s.active != nil {
		if call.Continuation {
			// A continuation call carries no content of its own to fold
			// in later (its Prompt is only a placeholder - see
			// continuationPromptPlaceholder); the completions that
			// triggered this attempt are still safe in the inbox for
			// whichever turn *is* active to pick up, either via its own
			// PrepareStep drain or via wakeFromInboxIfIdle once it goes
			// idle. Queuing this call would only let its placeholder be
			// persisted later as if it were a real follow-up (see
			// drainQueueForStep's fold, which calls createUserMessage on
			// whatever it finds) - drop it instead.
			//
			// Loud on purpose: this is the one place a call is ever
			// discarded outright rather than queued or run, and
			// startContinuation is the only constructor of a Continuation
			// call - if that ever stopped being true (a real prompt
			// mislabelled as a continuation, say), this is where it would
			// vanish with no persisted message and no terminal event. A
			// log line at least leaves a trace.
			slog.Debug("Dropping continuation call: another turn is already active", "session_id", call.SessionID)
			if call.Accepted != nil {
				call.Accepted.Close()
			}
			dispatched(SteerDropped)
			s.mu.Unlock()
			return dispatchOutcome{steer: SteerDropped}
		}
		// Busy: an earlier prompt is active. Queue this call so it is
		// folded into (or sequenced after) the active turn, and release any
		// accept reservation. A Cancel arriving after this point sees the
		// active entry and clears the queue.
		//
		// enqueueLocked strips OnComplete: the caller that supplied the
		// hook (typically coordinator.Run) has its own retry/coalesce
		// scope that ends when it returns, so by the time the queue
		// drains nobody is left to consume the buffered terminal event.
		// The queued turn falls back to the default broker publish,
		// which is what existing subscribers expect. It is called
		// directly, not via the self-locking enqueueCall, because this
		// goroutine already holds s.mu across the whole accept decision
		// and a sync.Mutex is not reentrant.
		//
		// A steering follow-up asked to reach the turn already in flight
		// rather than queue behind it, and the fold is keyed on the
		// absence of a RunID (see drainQueueForStep), so drop it here -
		// the one point where "the session is busy" is known atomically.
		// Nothing is left waiting on that RunID: the hook below tells the
		// caller its call was enqueued, precisely so it can settle its own
		// bookkeeping without a terminal event of its own. See
		// SessionAgentCall.Steering.
		if call.Steering && call.RunID != "" {
			slog.Debug("Dropping RunID from a steering call folding into the active turn", "session_id", call.SessionID, "run_id", call.RunID)
			call.RunID = ""
		}
		enqueueLocked(s, call)
		if call.Accepted != nil {
			call.Accepted.Close()
		}
		dispatched(SteerEnqueued)
		s.mu.Unlock()
		// enqueueLocked itself must not call notifyQueueChanged (it runs
		// under s.mu, held by this caller) - so this calls it explicitly,
		// only now that the lock is released.
		a.notifyQueueChanged(call.SessionID)
		// Release anything in the active turn that is only waiting (a
		// say) - but only for the person's own words. An
		// agent-originated prompt (a delegation follow-up queued behind
		// a turn already in flight) still stays queued; it just must
		// not cut short whatever the active turn is waiting on, or one
		// agent could use its own queued prompt to derail another
		// agent's work in flight - the same doctrine as
		// agent.WithSteering and thread.lifecycle's steer.
		if call.PromptOrigin != message.OriginAgent {
			a.signalUserInput(call.SessionID)
		}
		return dispatchOutcome{steer: SteerEnqueued}
	}

	// Idle: become the active run. Register the cancel func before dropping
	// the lock so a Cancel that arrives between here and assistant creation
	// is not lost.
	runCtx := context.WithValue(ctx, tools.SessionIDContextKey, call.SessionID)
	// Tools that wait on something other than the user can use this to
	// notice that the user has spoken and hand the turn back.
	runCtx = tools.WithUserInput(runCtx, func() <-chan struct{} {
		return a.userInputChan(call.SessionID)
	})
	genCtx, cancel := context.WithCancel(runCtx)
	ac := &activeCancel{cancel: cancel}
	s.active = ac
	// A new turn is genuinely starting for this session - whether a real
	// user Run/Steer or our own auto-continuation (see
	// enqueueCompletionAndDrainForWake) - so any earlier "user canceled
	// this session" marker is now stale.
	s.cancelled = false
	if call.Accepted != nil {
		call.Accepted.Close()
	}
	dispatched(SteerRan)
	s.mu.Unlock()

	return dispatchOutcome{steer: SteerRan, active: true, genCtx: genCtx, cancel: cancel, ac: ac}
}

// buildStreamAgent constructs the fantasy agent from an already assembled
// runtime. It deliberately does not read mutable agent or MCP state.
func (a *sessionAgent) buildStreamAgent(runtime streamRuntime) fantasy.Agent {
	return fantasy.NewAgent(
		runtime.model.Model,
		fantasy.WithSystemPrompt(runtime.systemPrompt),
		fantasy.WithTools(runtime.tools...),
		fantasy.WithUserAgent(userAgent),
	)
}

type streamRuntime struct {
	model                Model
	tools                []fantasy.AgentTool
	systemPrompt         string
	systemPromptPrefix   string
	disableAutoSummarize bool
	autoSummarizeAt      int64
}

// snapshotStreamRuntime captures the immutable runtime after readiness. It
// looks like a pointless forward to effectiveStreamRuntime, but it exists as
// an override seam: stream_runtime_snapshot_test.go's
// snapshotMutatingSessionAgent embeds *sessionAgent and shadows this method
// to mutate agent state between the snapshot and the provider call, which is
// what TestRunSubAgentUsesOneRuntimeSnapshotForBudgetAndProvider uses to
// prove a delegation's budget and provider request come from the same
// snapshot. Do not delete it or inline it into effectiveStreamRuntime.
func (a *sessionAgent) snapshotStreamRuntime(call SessionAgentCall) streamRuntime {
	return a.effectiveStreamRuntime(call)
}

// effectiveStreamRuntime is the single source of the per-turn prompt and tool
// snapshot. Budgeting uses the same assembly so it cannot reserve for tools or
// instructions that differ from the eventual Stream call.
func (a *sessionAgent) effectiveStreamRuntime(call SessionAgentCall) streamRuntime {
	if call.streamRuntime != nil {
		runtime := *call.streamRuntime
		runtime.tools = slices.Clone(runtime.tools)
		return runtime
	}
	runtime := streamRuntime{
		model:                a.model.Get(),
		tools:                a.tools.Copy(),
		systemPrompt:         a.systemPrompt.Get(),
		systemPromptPrefix:   a.systemPromptPrefix.Get(),
		disableAutoSummarize: a.disableAutoSummarize,
		autoSummarizeAt:      a.autoSummarizeAt,
	}
	if call.Runtime != nil {
		runtime.model = call.Runtime.model
		runtime.tools = slices.Clone(call.Runtime.tools)
		runtime.systemPrompt = call.Runtime.systemPrompt
		runtime.systemPromptPrefix = call.Runtime.systemPromptPrefix
		runtime.disableAutoSummarize = call.Runtime.disableAutoSummarize
		runtime.autoSummarizeAt = call.Runtime.autoSummarizeAt
	}
	runtime.tools = withoutUnusableParentTool(runtime.tools, a.dispatcher, call.SessionID)

	var instructions strings.Builder
	// a.mcp is nil for session agents built outside app.New; treat it as no MCP.
	if a.mcp != nil {
		for _, server := range a.mcp.GetStates() {
			if server.State != mcp.StateConnected {
				continue
			}
			if s := server.Client.InitializeResult().Instructions; s != "" {
				instructions.WriteString(s)
				instructions.WriteString("\n\n")
			}
		}
	}
	if s := instructions.String(); s != "" {
		runtime.systemPrompt += "\n\n<mcp-instructions>\n" + s + "\n</mcp-instructions>"
	}
	return runtime
}

// withoutUnusableParentTool drops ask_parent from a turn's tool list when
// the session running it has no registered parent to message.
//
// The coordinator cannot make this call: buildTools runs once per
// coordinator, not per session, and a task shares its parent's exact
// coordinator and tool list, so there is no build-time list that
// distinguishes a delegation's own session from its parent's (see
// coordinator.buildTools). ask_parent was therefore offered to every
// top-level turn as well, where it can only fail -- and a model that is
// handed a tool tends to reach for it. One did: a top-level session
// called ask_parent to send instructions *down* to a thread it had
// created, got "no registered parent to message", and spent the turn on
// nothing. Deciding it here, where the session id is known, is the only
// place the answer exists.
//
// Order is preserved, which matters more than it looks: the fixed
// provider policy stamps the cache-control breakpoint onto the last tool
// in the list (see NewSessionAgent), and buildTools sorts tools by name,
// so ask_parent sits near the front and removing it cannot move that
// breakpoint.
func withoutUnusableParentTool(agentTools []fantasy.AgentTool, dispatch *dispatcher, sessionID string) []fantasy.AgentTool {
	if dispatch == nil {
		return agentTools
	}
	if _, hasParent := dispatch.delegationParents.Get(sessionID); hasParent {
		return agentTools
	}
	idx := slices.IndexFunc(agentTools, func(t fantasy.AgentTool) bool {
		return t.Info().Name == tools.AskParentToolName
	})
	if idx < 0 {
		return agentTools
	}
	return slices.Delete(slices.Clone(agentTools), idx, idx+1)
}

// recordSessionModel pins sessionID to the model this agent is currently
// running, so a later restore of that session can put the model back. See
// session.Session.Model.
//
// Failures are logged and swallowed: this is bookkeeping alongside a turn
// that is already starting, and a session that keeps no model behaves the
// way every session behaved before the column existed — it falls back to
// the instance's own selection.
func (a *sessionAgent) recordSessionModel(ctx context.Context, call SessionAgentCall) {
	if a.sessions == nil {
		return
	}
	sessionID := call.SessionID
	cfg := a.callModel(call).ModelCfg
	if cfg.Provider == "" || cfg.Model == "" {
		// Nothing worth recording, and writing the zero ref here would
		// clear a pin the session legitimately has.
		return
	}
	if err := a.sessions.SetModel(ctx, sessionID, session.ModelRef{
		Provider: cfg.Provider,
		Model:    cfg.Model,
	}); err != nil {
		slog.Error("Failed to record the model a session ran on",
			"component", "agent", "session_id", sessionID, "error", err)
	}
}

// finishTurn concludes a turn whose Stream call returned without error: it
// runs auto-summarize if the turn tripped the context-window threshold,
// releases this turn's active-request slot, fires the finished notification,
// and hands off to the next queued call, if any.
//
// It is runTurn's only source of a non-nil next call, which is what lets
// run hand off to it with a loop instead of tail recursion: runTurn
// returns next unchanged to its caller, which loops instead of calling
// itself.
//
// reporter is the single owner of this call's terminal RunComplete: every
// return path here either lets runTurn's streaming defer publish naturally
// (the common case, and the two error returns below), or explicitly spends
// reporter's Once first — via publish (a different queued call is taking
// over, so this call's own RunID is genuinely done) or suppress (this same
// call, under the same RunID, is being re-queued to resume after a summary,
// so a later turn owns the terminal event instead). Because reporter is
// backed by sync.Once, at most one of those ever actually emits, regardless
// of which path is taken - the invariant holds by construction, not by the
// caller getting the branches right.
func (a *sessionAgent) finishTurn(
	ctx, genCtx context.Context,
	call SessionAgentCall,
	ac *activeCancel,
	cancel context.CancelFunc,
	t *runTurn,
	reporter *completionReporter,
	result *fantasy.AgentResult,
	err error,
) (*fantasy.AgentResult, *SessionAgentCall, error) {
	// summarizeFailed, when set below, is reported on this turn's own
	// RunComplete rather than by returning early: see the shouldSummarize
	// branch's comment.
	var summarizeFailed error
	if t.shouldSummarize {
		// Hand our still-installed active-run slot straight to summarize
		// via claim, rather than releasing it first: releasing here and
		// letting summarize claim its own afterward left a window where a
		// queued continuation could claim the session first, turning this
		// successful turn's summarize into an ErrSessionBusy. summarize
		// swaps the slot atomically instead.
		// summarize derives its provider-log correlation from its context. A
		// SessionAgentCall may be constructed directly (without WithRunID on
		// the parent context), and queued calls can carry a RunID different
		// from the context that originally started the parent turn. Stamp this
		// call's RunID explicitly so the summary belongs to the turn that
		// triggered it.
		summarizeCtx := WithRunID(genCtx, call.RunID)
		// call.OnRateLimit is whatever this turn already rotates with: the
		// coordinator's for a person's turn, the sub-agent's for a
		// delegation. Passing it through keeps the summary on the same
		// account discipline as the turn that triggered it.
		if summarizeErr := a.summarize(summarizeCtx, call.SessionID, call.ProviderOptions, call.OnAuthRefresh, call.OnRateLimit, t.model, t.promptPrefix, call.ActiveRuntime, ac); summarizeErr != nil {
			// Not fatal to this turn's completion bookkeeping: the turn
			// itself already streamed successfully, and there is no
			// summary to resume a continuation from, so log and fall
			// through to the same release/notify/drain teardown every
			// other turn gets - mirroring how handleStreamError treats a
			// failed synthetic-tool-result write as non-fatal to reaching
			// its own finish handling. Returning early here would skip
			// both the AgentFinished notification and the drainNext
			// handoff, leaving a queued RunID-bearing prompt behind this
			// session stuck forever and its caller blocked on RunComplete
			// indefinitely. summarizeFailed is reported below via this
			// turn's own RunComplete instead.
			slog.Error("Failed to summarize session after turn", "session_id", call.SessionID, "error", summarizeErr)
			summarizeFailed = summarizeErr
		} else if len(t.currentAssistant.ToolCalls()) > 0 && !t.haltedByTool {
			// If the agent wasn't done...
			//
			// t.haltedByTool excludes the step a hook Halt, a permission
			// denial, or a pending question stopped: fantasy's own
			// StopWhen conditions run regardless of that halt (see
			// third_party/fantasy/agent.go), so shouldSummarize can still
			// trip on the very step that was told to stop - and
			// hooked_tool.go documents Halt as ending the whole turn, not
			// pausing it. Summarizing still happens above (freeing the
			// context is worth doing either way); only the requeue -
			// which would silently resume a turn the halt meant to end,
			// with a fabricated "session was interrupted" prompt nobody
			// asked for - is skipped.
			//
			// A continuation's prompt is not a prompt: it is the
			// placeholder its own step 0 verifies and strips (see
			// continuationPromptPlaceholder). Rewriting it would break
			// that invariant — the resumed turn, still flagged
			// Continuation, would fail at step 0 with "does not match
			// the expected placeholder text", stopping the agent
			// outright, and would tell the model its "initial user
			// request" was a placeholder string. A continuation resumes
			// on the summary, which is what it was going to read anyway.
			if !call.Continuation {
				call.Prompt = fmt.Sprintf("The previous session was interrupted because it got too long, the initial user request was: `%s`", call.Prompt)
			}
			a.requeueContinuation(call, reporter.suppress)
		}
	}

	return a.completeTurn(ctx, call, ac, cancel, t, reporter, result, err, summarizeFailed)
}

// completeTurn runs the release/notify/drain/hand-off tail every terminal
// turn outcome needs, regardless of whether Stream itself succeeded:
// releases this turn's active-request slot, fires AgentFinished, and hands
// off to the next queued call (if any) so a prompt queued behind this turn
// - including one carrying its own RunID - is never stranded waiting on a
// RunComplete that would otherwise never come.
//
// finishTurn (the Stream-success path) and runTurn's Stream-error path (a
// non-cancel error never reaches finishTurn at all) both funnel through
// here for exactly that reason - see
// TestRunTurn_StreamErrorStillDrainsQueue.
//
// summarizeFailed, when non-nil, is folded into this turn's own terminal
// RunComplete exactly as finishTurn's shouldSummarize branch does; the
// Stream-error caller has no summarize step of its own and passes nil.
func (a *sessionAgent) completeTurn(
	ctx context.Context,
	call SessionAgentCall,
	ac *activeCancel,
	cancel context.CancelFunc,
	t *runTurn,
	reporter *completionReporter,
	result *fantasy.AgentResult,
	err error,
	summarizeFailed error,
) (*fantasy.AgentResult, *SessionAgentCall, error) {
	// Release active request before publishing the notification.
	// TUI handlers poll IsSessionBusy() and only re-evaluate when a
	// tea.Msg arrives, so the cleanup must precede the notify or
	// subscribers see stale busy state at the moment of receipt.
	//
	// Conditional release, like the shouldSummarize one above: when that
	// branch already cleared our own entry, this is a no-op against
	// whatever newer run's entry (if any) is there now. Otherwise ac is
	// still our own entry here (nothing else clears it first), so this
	// behaves exactly like the unconditional Del it replaces. Either way,
	// our own entry can't leak: we only ever remove it, never re-register
	// it, after this point.
	a.clearActiveIfMatch(call.SessionID, ac)
	cancel()

	// summarizeFailed's context.Canceled case is a user Escape landing
	// mid-auto-summarize (summarize's own genCtx is derived from this
	// turn's genCtx - see finishTurn), not a failure this turn should
	// report as one. AgentDispatcher.run's TypeAgentError path (fired on
	// err != nil, see completeTurn's other caller in run_turn.go) already
	// owns the failure case, so publishing TypeAgentFinished here too
	// would double up "Task finished" with "Task failed" for the same
	// turn - see internal/app/agent_dispatch.go's run and
	// internal/ui/model/notifications.go's handleAgentNotification.
	summarizeCancelled := errors.Is(summarizeFailed, context.Canceled)

	// Send notification that agent has finished its turn (skip for
	// nested/non-interactive sessions, a failed Stream, or a
	// summarize the user canceled).
	if !call.NonInteractive && a.notify != nil && err == nil && !summarizeCancelled {
		a.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			SessionID:    call.SessionID,
			SessionTitle: t.currentSession.Title,
			Type:         notify.TypeAgentFinished,
		})
	}

	// Hand off to the next queued prompt (if any). drainNext filters the
	// queue against the cancel mark and reserves a fresh accept for the
	// survivor under the same per-session dispatch lock used throughout,
	// keeping the session observable to Cancel for the entire transition
	// and closing the dequeue -> re-register window.
	queuedMessages, firstQueued, canceledRunIDDrops := a.drainNext(call.SessionID)
	// A dropped prompt carrying a RunID must still publish its terminal
	// cancelled RunComplete so a caller waiting on that RunID does not
	// hang.
	a.publishCanceledQueueDrops(canceledRunIDDrops)
	if firstQueued == nil {
		// Nothing queued behind this turn: report a summarize failure (if
		// any) on this turn's own terminal event now, since there is no
		// later turn under this RunID left to carry it. reporter.publish
		// is a no-op when summarizeFailed is nil - runTurn's own deferred
		// fallback publisher takes care of the normal case.
		if summarizeFailed != nil {
			complete := notify.RunComplete{SessionID: call.SessionID, RunID: call.RunID, Error: summarizeFailed.Error()}
			complete.Cancelled = summarizeCancelled
			// A cancel (Escape) that lands mid-auto-summarize surfaces
			// here as summarizeFailed wrapping context.Canceled, and
			// without Cancelled set the caller (see thread/lifecycle.go)
			// reads a non-empty Error as StatusFailed instead of a plain
			// cancellation. The outerOwesRunComplete branch below applies
			// the same rule; both are needed, since which one publishes
			// depends only on whether a prompt was waiting in the queue.
			if t.currentAssistant != nil {
				complete.MessageID = t.currentAssistant.ID
				complete.Text = t.currentAssistant.Content().String()
			}
			if ctx.Err() != nil {
				complete.Cancelled = true
			}
			reporter.publish(ctx, complete)
		}
		return result, nil, err
	}

	// Decide whether this turn still owes its own terminal RunComplete
	// before handing off to firstQueued. Each submitted prompt with a
	// RunID has its own lifecycle, so a turn that is finished and handing
	// off to a *different* queued prompt must publish its own RunComplete
	// here — leaving it to a later turn (which may carry a different
	// RunID, or none) would hang a caller waiting on this turn's RunID.
	// The exception is the summarize-continuation path just above, which
	// re-queues this same call (same RunID) to resume after a summary:
	// queuedMessages then still contains an entry for call.RunID (it need
	// not be firstQueued itself - a genuinely different prompt queued
	// ahead of the re-queued continuation takes that slot), so the loop
	// below finds it and this turn suppresses instead of publishing; the
	// eventual terminal turn for this RunID publishes on its own pass
	// through here.
	outerOwesRunComplete := call.RunID != ""
	if outerOwesRunComplete {
		for _, q := range queuedMessages {
			if q.RunID == call.RunID {
				outerOwesRunComplete = false
				break
			}
		}
	}
	if outerOwesRunComplete {
		complete := notify.RunComplete{SessionID: call.SessionID, RunID: call.RunID}
		// err carries this turn's own Stream failure (the finishTurn
		// caller always passes nil here, since it is only reached after
		// Stream succeeded); summarizeFailed carries a failure from the
		// summarize step that ran after a successful Stream. The two are
		// mutually exclusive in practice, but check err first so a real
		// Stream failure is never masked.
		if err != nil {
			complete.Error = err.Error()
			complete.Cancelled = errors.Is(err, context.Canceled)
		} else if summarizeFailed != nil {
			complete.Error = summarizeFailed.Error()
			// Same rule as the branch above, and as the no-queue branch:
			// an Escape landing mid-auto-summarize arrives here wrapping
			// context.Canceled, and a consumer reading a non-empty Error
			// without Cancelled calls it a failure. thread/lifecycle.go
			// does exactly that, so the thread finalized as
			// "failed: context canceled". ctx.Err() below does not cover
			// it: a cancel takes down the turn's genCtx, not the outer
			// ctx this reads.
			complete.Cancelled = errors.Is(summarizeFailed, context.Canceled)
		}
		if t.currentAssistant != nil {
			complete.MessageID = t.currentAssistant.ID
			complete.Text = t.currentAssistant.Content().String()
		}
		if ctx.Err() != nil {
			complete.Cancelled = true
		}
		reporter.publish(ctx, complete)
	} else {
		reporter.suppress()
	}
	return result, firstQueued, err
}

// runTurn runs exactly one turn attempt for call: the dispatch decision,
// the stream (if this call became the active run), and — on success —
// finishTurn's summarize/handoff tail. next is non-nil only when the turn
// handed off to a queued call; run's loop takes that as the next call to
// attempt instead of recursing, which is what keeps the loop's stack depth
// flat no matter how long the queue behind a session gets.
func (a *sessionAgent) runTurn(ctx context.Context, call SessionAgentCall) (outcome SteerOutcome, result *fantasy.AgentResult, next *SessionAgentCall, retErr error) {
	decision := a.dispatchDecision(ctx, call)
	if !decision.active {
		return decision.steer, decision.result, nil, decision.err
	}
	genCtx, cancel, ac := decision.genCtx, decision.cancel, decision.ac

	// Record the model this turn is about to run on, so restoring the
	// session later restores the model it was working with rather than
	// whatever the instance has selected then. Stamped here, from the one
	// point where a turn genuinely becomes this session's active run, so
	// what is recorded is what the session did rather than what anyone
	// intended — a queued prompt that drains into a turn of its own needs
	// no stamp of its own, since it runs on the same model that put it
	// here.
	//
	// Best-effort on purpose: a session whose model fails to persist still
	// runs the turn normally - failing it over this bookkeeping would be
	// the worse trade.
	a.recordSessionModel(ctx, call)

	// reporter is this turn's single owner of the terminal RunComplete
	// event (see completionReporter's own doc comment). It is
	// constructed here, the moment this call has genuinely become the
	// active run, rather than after the fallible session/message setup
	// below - a session.Get, getSessionMessages, or createUserMessage
	// failure returning before any reporter exists would silently
	// discard the prompt: nothing persisted, and no terminal event for a
	// caller waiting on one. See the fallback defer below.
	reporter := newCompletionReporter(a, call)
	// t stays nil until newRunTurn runs, after createUserMessage
	// succeeds; the deferred publish below tolerates that - a failure
	// before then never produced an assistant message to report.
	var t *runTurn

	defer cancel()
	// Conditional cleanup: only remove our entry if it hasn't been replaced
	// by a newer run. Without this guard, the deferred Del fires after a
	// concurrent run registers in the completion window, silently wiping
	// the new run's cancel and breaking cancellation.
	//
	// wakeFromInboxIfIdle runs right after: this is the single choke
	// point every exit from here (success, error, cancel, panic) passes
	// through exactly once, right when this session's activeRequests
	// entry has just been cleared (whether by this CompareAndDelete or,
	// for the shouldSummarize/queued-handoff paths, by an earlier
	// explicit one this call becomes a harmless no-op against). Without
	// it, a completion that arrived while this turn was busy but never
	// got another step of its own (the common case: most turns are a
	// single step) would sit in the inbox with nothing left to drain it
	// until some unrelated later call happened to touch this session —
	// exactly the race the "wake an idle parent" contract calls out.
	defer func() {
		a.clearActiveIfMatch(call.SessionID, ac)
		a.wakeFromInboxIfIdle(context.WithoutCancel(ctx), call.SessionID)
	}()
	// MessageService already flushes synchronously on terminal updates;
	// the defer guarantees it at every runTurn exit without callers
	// needing to know, and publishes the authoritative RunComplete for
	// this turn after the flush. reporter's Once makes this the fallback
	// publisher: a no-op wherever finishTurn already spent it explicitly
	// (finishTurn only does so itself when handing off to a queued call
	// under a different RunID - see its own "outerOwesRunComplete"
	// comment - so for the common case of a turn with nothing queued
	// behind it, this deferred call is the *only* publisher).
	//
	// Registered here, immediately once this call has genuinely become
	// the active run, rather than after the fallible session/message
	// setup below (session.Get, getSessionMessages, createUserMessage):
	// a failure in any of those returning before any such defer exists
	// would silently discard the prompt - nothing persisted, and no
	// terminal event for a caller waiting on one.
	defer func() {
		// Use a context detached from the run context: workspace
		// shutdown cancels ctx before this goroutine returns, but the
		// buffered streaming deltas must still land before the DB is
		// closed. A short timeout bounds the flush.
		flushCtx, flushCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer flushCancel()
		if flushErr := a.messages.FlushAll(flushCtx); flushErr != nil {
			slog.Error("Failed to flush pending message updates after run", "error", flushErr)
		}
		complete := notify.RunComplete{SessionID: call.SessionID, RunID: call.RunID}
		if t != nil && t.currentAssistant != nil {
			complete.MessageID = t.currentAssistant.ID
			complete.Text = t.currentAssistant.Content().String()
		}
		if retErr != nil {
			complete.Error = retErr.Error()
			complete.Cancelled = errors.Is(retErr, context.Canceled)
		} else if ctx.Err() != nil {
			complete.Cancelled = true
		}
		// Publish on a context detached from the run's, not on ctx:
		// workspace shutdown may have already cancelled ctx by the time
		// this defer runs, and publish drops the terminal event against
		// an already-cancelled context, leaving a subscriber waiting on
		// this RunID to hang until its own timeout. It gets its own
		// budget rather than reusing flushCtx, whose 5s may already be
		// spent by the flush above; the bound still keeps a publish to
		// a subscriber that never receives from blocking forever.
		publishCtx, publishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer publishCancel()
		reporter.publish(publishCtx, complete)
	}()

	runtime := a.effectiveStreamRuntime(call)
	streamAgent := a.buildStreamAgent(runtime)
	model, agentTools, promptPrefix := runtime.model, runtime.tools, runtime.systemPromptPrefix
	summarize := summarizePolicy{disabled: runtime.disableAutoSummarize, at: runtime.autoSummarizeAt}

	currentSession, err := a.sessions.Get(ctx, call.SessionID)
	if err != nil {
		return SteerRan, nil, nil, fmt.Errorf("failed to get session: %w", err)
	}

	msgs, err := a.getSessionMessages(ctx, currentSession)
	if err != nil {
		return SteerRan, nil, nil, fmt.Errorf("failed to get session messages: %w", err)
	}

	// A sub-agent session already got a deliberate title from
	// CreateSubAgentSession (the delegation description, or a fallback
	// like "New Agent Session") — never a blank one, so an empty title
	// here means an unusual construction path that skipped that step,
	// and generateTitle is the only thing left that will ever set one.
	// Only skip when both hold: sub-agent *and* already titled.
	skipTitle := a.isSubAgent && currentSession.Title != ""
	if !call.Continuation && !hasUserTextMessage(msgs) && !skipTitle {
		a.startGenerateTitle(ctx, call.SessionID, call.Prompt, model, promptPrefix)
	}

	var userMsgCreated bool
	if !call.Continuation {
		_, err = a.createUserMessage(ctx, call)
		if err != nil {
			return SteerRan, nil, nil, err
		}
		userMsgCreated = true
	}

	ctx = context.WithValue(ctx, tools.SessionIDContextKey, call.SessionID)
	t = newRunTurn(a, call, ctx, genCtx, model, agentTools, promptPrefix, summarize, currentSession, userMsgCreated)

	// Carried-over history goes in front of this session's own
	// messages.
	history, files := a.preparePrompt(withPriorMessages(call.PriorMessages, msgs), model.CatalogCfg.SupportsImages, currentSession.Todos, call.Attachments,
		withRepairSessionID(call.SessionID, call.RunID),
		withRepairOrigins(turnOrigins(call.PriorMessages, msgs)),
	)

	// Only this session's own messages, not the carried ones: a summary
	// replaces exactly what msgs holds, which is what stopOnContextWindow
	// needs to know to tell a summarizable context from an irreducible
	// one. This pass is purely for the token estimate - it is not the prompt
	// the model sees - so it suppresses the orphan-repair diagnostics: the
	// main history pass above already logged (and counted) the orphans this
	// pass would meet again, and a second line would not correspond to a
	// request actually sent.
	ownHistory, _ := a.preparePrompt(msgs, model.CatalogCfg.SupportsImages, currentSession.Todos, nil,
		withRepairSuppressed(),
	)
	t.historyTokens = estimateMessageTokens(ownHistory)

	// Don't send MaxOutputTokens if 0 — some providers (e.g. LM Studio) reject it
	var maxOutputTokens *int64
	if call.MaxOutputTokens > 0 {
		maxOutputTokens = &call.MaxOutputTokens
	}
	// onAuthRefresh is the turn's OnAuthRefresh: it wraps the call's
	// (coordinator's) credential refresh so a *successful* refresh marks the
	// next attempt as an auth_refresh request. When the call carries no
	// refresh (the in-memory/continuation path), pass nil so fantasy does not
	// even attempt a refresh - the wrapper must not run and pretend a
	// refresh succeeded when none was configured.
	var turnOnAuthRefresh fantasy.OnAuthRefreshFunc
	if call.OnAuthRefresh != nil {
		turnOnAuthRefresh = t.onAuthRefresh
	}
	// Same pattern as turnOnAuthRefresh above: only wire the wrapper when
	// the coordinator actually configured a rotation callback (rotation
	// enabled for a RotateRateLimit provider), so fantasy's OnRateLimit
	// hook stays nil - and therefore never even inspected in the retry
	// loop - whenever rotation is off. See runtimeBuilder.rotatorFor.
	var turnOnRateLimit fantasy.OnRateLimitFunc
	if call.OnRateLimit != nil {
		turnOnRateLimit = t.onRateLimit
	}
	defer func() {
		if t != nil {
			t.requeuePendingCompletions()
		}
	}()
	result, err = streamAgent.Stream(genCtx, fantasy.AgentStreamCall{
		Prompt:           message.PromptWithTextAttachments(call.Prompt, call.Attachments),
		Files:            files,
		Messages:         history,
		Headers:          sessionHeaders(call.SessionID),
		ProviderOptions:  call.ProviderOptions,
		MaxOutputTokens:  maxOutputTokens,
		TopP:             call.TopP,
		Temperature:      call.Temperature,
		PresencePenalty:  call.PresencePenalty,
		TopK:             call.TopK,
		FrequencyPenalty: call.FrequencyPenalty,
		PrepareStep:      t.prepareStep,
		OnReasoningStart: t.onReasoningStart,
		OnReasoningDelta: t.onReasoningDelta,
		OnReasoningEnd:   t.onReasoningEnd,
		OnTextDelta:      t.onTextDelta,
		OnToolInputStart: t.onToolInputStart,
		OnToolInputDelta: t.onToolInputDelta,
		OnRetry:          t.onRetry,
		OnAuthRefresh:    turnOnAuthRefresh,
		OnRateLimit:      turnOnRateLimit,
		ModelProvider:    t.modelProvider,
		OnToolCall:       t.onToolCall,
		OnToolResult:     t.onToolResult,
		OnStepFinish:     t.onStepFinish,
		StopWhen: []fantasy.StopCondition{
			t.stopOnContextWindow,
			func(steps []fantasy.StepResult) bool {
				return hasRepeatedToolCalls(steps)
			},
		},
	})
	if err != nil {
		streamResult, streamErr := t.handleStreamError(err)
		// t.currentAssistant == nil means PrepareStep failed, or never ran,
		// before step 0's assistant message existed -
		// handleStreamError's own "before assistant" branch
		// (handleStreamErrorBeforeAssistant). Two distinct paths land
		// here, and errors.Is(err, context.Canceled) tells them apart:
		//
		//   - A genuine cancel in the window between dispatchDecision
		//     registering the active run and PrepareStep creating the
		//     assistant message: all of runTurn's setup up to Stream
		//     (session.Get, createUserMessage, ...) runs on ctx, not
		//     genCtx, so an Escape landing there surfaces here rather
		//     than in the cancel branch below. A prompt typed right
		//     after that Escape still queues behind this turn like any
		//     other - and nothing else will ever look at this session's
		//     queue once Stream returns, since wakeFromInboxIfIdle does
		//     not consult it - so it must be drained here too.
		//   - foldSteering's own PrepareStep rollback (turn.go): a
		//     non-cancel persist failure, which re-queues the
		//     unpersisted remainder of THIS call's own folded
		//     follow-ups itself before returning its error, so that
		//     queue already holds this same failure's own rollback.
		//     Draining it here would hand off to that rollback and
		//     swallow this call's own error return - its only
		//     completion channel when call.RunID is empty - so this
		//     path returns without draining.
		if t.currentAssistant == nil {
			if errors.Is(err, context.Canceled) {
				_, next, canceledRunIDDrops := a.drainNext(call.SessionID)
				a.publishCanceledQueueDrops(canceledRunIDDrops)
				return SteerRan, streamResult, next, streamErr
			}
			return SteerRan, streamResult, nil, streamErr
		}
		// dispatcher.Cancel only clears what was queued at the instant it
		// fired. A prompt that arrives between that moment and Stream
		// actually observing the cancellation - a tool slow to notice ctx, or
		// the DB writes above - queues behind this turn like any other, and
		// nothing else will ever look at this session's queue once a
		// canceled Stream returns, so it must be drained here.
		//
		// drainNext, not completeTurn: this turn's own RunComplete - which
		// must report Cancelled - is already guaranteed by the deferred
		// publisher above, and no AgentFinished is owed for a turn that was
		// canceled rather than finished.
		if errors.Is(err, context.Canceled) {
			_, next, canceledRunIDDrops := a.drainNext(call.SessionID)
			a.publishCanceledQueueDrops(canceledRunIDDrops)
			return SteerRan, streamResult, next, streamErr
		}
		// A non-cancel Stream error (a genuinely failed request, not a
		// user cancellation) must not return here directly: skipping the
		// release/notify/drain tail would leave a prompt queued behind
		// the failed turn (including a RunID-bearing `sennit run` caller)
		// stuck in the queue with no hand-off until its own timeout.
		// completeTurn is the same tail finishTurn's Stream-success path
		// runs.
		result, next, retErr = a.completeTurn(ctx, call, ac, cancel, t, reporter, streamResult, streamErr, nil)
		return SteerRan, result, next, retErr
	}

	result, next, retErr = a.finishTurn(ctx, genCtx, call, ac, cancel, t, reporter, result, err)
	return SteerRan, result, next, retErr
}

// run is the shared implementation behind the exported Run and Steer: both
// dispatch through it so there is exactly one place that makes the
// cancel-on-entry / enqueue / become-active decision and exactly one
// per-session lock discipline guarding it. Run discards outcome; Steer
// reports it. See SteerOutcome.
//
// A loop, not tail recursion, hands a queued call off to the next
// runTurn: recursion would land the recursive call's return values in
// this function's own named returns before the deferred publish ran
// (needing completionReporter's Once to paper over it), and an
// arbitrarily long queue behind a busy session would grow the goroutine
// stack by one frame per hop. Looping avoids both: each call to runTurn
// is a self-contained function invocation with its own named returns and
// its own defers, so there is nothing left for a later hop to clobber,
// and the stack does not grow with the queue's length.
func (a *sessionAgent) run(ctx context.Context, call SessionAgentCall) (SteerOutcome, *fantasy.AgentResult, error) {
	for {
		if err := ValidateCall(call); err != nil {
			return SteerRan, nil, err
		}
		outcome, result, next, err := a.runTurn(ctx, call)
		if next == nil {
			return outcome, result, err
		}
		call = *next
	}
}

// Run dispatches call, atomically picking cancel-on-entry, enqueue
// behind an active turn, or become the active turn, under
// call.SessionID's per-session dispatch mutex - see run. A queued call
// and a turn that legitimately produced no result both return (nil,
// nil), so a caller that needs to tell those apart should use Steer
// instead.
func (a *sessionAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	_, result, err := a.run(ctx, call)
	return result, err
}

// runWithStreamRuntime is the internal delegation entry point. The supplied
// runtime was captured after readiness and is never reassembled by Run.
func (a *sessionAgent) runWithStreamRuntime(ctx context.Context, call SessionAgentCall, runtime streamRuntime) (*fantasy.AgentResult, error) {
	call.streamRuntime = &runtime
	return a.Run(ctx, call)
}

// Steer gives interactive follow-ups an explicit entry point for the
// "enqueue behind the active turn, or start a new one" decision that Run
// already makes as a side effect of its busy check, and - unlike Run -
// reports which one happened. Steer does not reimplement that decision;
// it dispatches through the same run as Run, so it inherits the exact
// same atomicity: the busy check and the activeRequests registration (or
// the enqueue) happen under call.SessionID's per-session dispatch mutex,
// the same lock run's own end-of-turn cleanup and drainNext handoff use.
// A Steer call can only ever land on one side of that lock - observing
// the session as busy (and getting queued for the active turn to drain
// or hand off) or as idle (and becoming the new active turn) - never
// both and never neither, so a follow-up landing exactly as the active
// turn finishes is still guaranteed to run exactly once, and Steer's
// reported outcome always matches what actually happened to it.
//
// The RunID fork is entirely run's (see drainQueueForStep/drainNext): a
// queued call without a RunID folds into the active turn's next step;
// one with a RunID always gets its own turn and its own RunComplete,
// whether that turn starts immediately (idle) or via the recursive
// hand-off (busy). Steer does not special-case RunID either way.
func (a *sessionAgent) Steer(ctx context.Context, call SessionAgentCall) (SteerOutcome, *fantasy.AgentResult, error) {
	return a.run(ctx, call)
}
