// Package agent is the core orchestration layer for Braid AI agents.
//
// It provides session-based AI agent functionality for managing
// conversations, tool execution, and message handling. It coordinates
// interactions between language models, messages, sessions, and tools while
// handling features like automatic summarization, queuing, and token
// management.
package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/agent/notify"
	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/agent/tools/mcp"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/version"
)

var userAgent = fmt.Sprintf("Charm-Braid/%s (https://charm.land/braid)", version.Version)

type SessionAgentCall struct {
	SessionID string
	// RunID, when non-empty, is the caller-supplied correlator that
	// gets echoed back on the notify.RunComplete event emitted for
	// this turn. It is preserved when the call is enqueued behind a
	// busy session so the queued turn's terminal event is still
	// recognisable to the original caller. Callers that need a
	// reliable completion contract (e.g. `braid run` against a
	// session that may be busy) MUST set it; SessionID alone is
	// ambiguous when concurrent turns share the same session.
	RunID  string
	Prompt string
	// PromptOrigin is the message.Origin Prompt should be persisted
	// under by createUserMessage. Empty means the default,
	// message.OriginPerson — set only by coordinator.run reading
	// PromptOriginFromContext(ctx), which in turn is set by thread/task
	// dispatch (a delegation's goal, or a thread_send/task_send
	// follow-up; see agent.WithPromptOrigin). Role is always
	// message.User regardless of PromptOrigin — origin is authorship
	// metadata only and has zero effect on what reaches the model (see
	// Message.ToAIMessage).
	PromptOrigin message.Origin
	// PriorMessages is conversation history from *other* sessions that
	// this call should see ahead of its own. Only a named sub-agent
	// delegation sets it: each delegation gets a fresh session (the UI
	// addresses a delegation block by its session id), so without this
	// the same named agent would start from nothing every time it is
	// called. See coordinator.carryOverMessages.
	//
	// These messages are prepended to the model's view of history and
	// nothing else: they are not persisted into this session, not
	// counted when deciding whether to generate a title, and never
	// summarized - the sessions they came from own them.
	PriorMessages    []message.Message
	ProviderOptions  fantasy.ProviderOptions
	Attachments      []message.Attachment
	MaxOutputTokens  int64
	Temperature      *float64
	TopP             *float64
	TopK             *int64
	FrequencyPenalty *float64
	PresencePenalty  *float64
	NonInteractive   bool
	// OnComplete, when non-nil, replaces the default RunComplete
	// publish path: the inner Run hands the terminal payload to this
	// callback instead of emitting it on the RunComplete broker. The
	// coordinator uses this hook to coalesce the unauthorized →
	// re-auth → retry chain into a single user-visible terminal
	// event, so non-interactive clients (e.g. `braid run`) don't
	// exit on a stale failed-attempt RunComplete before the
	// successful retry. It is intentionally stripped when queueing
	// a busy-session call (see Run): the originating
	// coordinator.Run has long returned by the time the queued
	// recursion drains, so falling back to the default broker
	// publish keeps the event visible to subscribers.
	OnComplete func(notify.RunComplete)
	// Accepted, when non-nil, is the accept reservation taken by
	// BeginAccepted before the call was dispatched onto a goroutine
	// (the client/server fire-and-forget path). Run consumes it under
	// dispatchMu[SessionID] once the accepted -> (cancel-on-entry |
	// queued | active) transition has been chosen. When nil
	// (in-process / local callers like AppWorkspace), behavior is
	// unchanged and no accept tracking applies.
	Accepted *AcceptedRun
	// acceptSeq carries the accept sequence of the handle that produced
	// this call after it has been enqueued and its Accepted handle
	// stripped. The queue-drain paths compare it against a session's
	// cancel mark so a follow-up queued before a cancel is dropped while
	// one queued after the cancel survives. 0 means untracked (an
	// in-process enqueue with no accept reservation), which the drain
	// paths treat as covered by any present mark, preserving the
	// pre-sequence behavior.
	acceptSeq uint64
	// queuedAt is when this call was enqueued behind a busy session (see
	// dispatcher.enqueueCall, the only place it is set) — the "submit"
	// instant runTurn.prepareStep's steering fold measures against once
	// it drains and folds this call into a step. Zero for a call that
	// was never queued (ran as its own turn immediately) or was queued
	// by some path other than enqueueCall (e.g. requeueContinuation's
	// post-summarize resumption, which is the same turn continuing, not
	// a steering follow-up) — the fold log skips zero entries rather
	// than reporting a nonsense multi-decade wait.
	queuedAt time.Time
	// OnAuthRefresh, when non-nil, is called by fantasy when a stream
	// fails with an authentication error (HTTP 401). The callback should
	// refresh credentials and return nil on success, in which case
	// fantasy retries the stream transparently. Returning an error
	// surfaces the original auth error without retry.
	OnAuthRefresh func(ctx context.Context, err *fantasy.ProviderError) error
	// Runtime carries the model and tools selected by the coordinator for
	// this lifecycle. It is preserved across queued continuations.
	Runtime       *compiledRuntime
	ActiveRuntime *activeRuntime
	// Depth is this turn's background-delegation cascade depth: 0 for a
	// real user turn (the zero value, so every ordinary caller gets this
	// for free). For a Continuation call it starts at 0 too and is set by
	// PrepareStep once it knows what it actually drained (N+1 for a
	// depth-N completion) - see runTurn.prepareStep. Either way,
	// PrepareStep stamps the final value onto the step's context
	// (tools.DepthContextKey) so the "agent" tool's background mode can
	// refuse to start further background work once it reaches the hard
	// cascade limit.
	Depth int
	// Continuation marks an auto-woken continuation turn (see
	// startContinuation): its Prompt is a placeholder, not real content
	// (see continuationPromptPlaceholder), so run() skips
	// createUserMessage for it and PrepareStep strips the placeholder
	// message before the model ever sees it, folding in whatever
	// PrepareStep's own completion-inbox drain finds instead - the same
	// mechanism and the same non-persisted delivery the mid-turn fold
	// case already uses, so the two paths cannot produce different
	// history for the same event.
	Continuation bool
}

// SteerOutcome reports which branch of sessionAgent.run's atomic dispatch
// decision a call took. Run discards it (a queued call and a completed
// call with no result both look like (nil, nil) to a Run caller); Steer
// exists specifically to hand it back, so a caller can tell "queued
// behind the active turn" from "ran as its own turn" without a
// follow-up probe of the queue.
type SteerOutcome int

const (
	// SteerRan means the call became a turn itself (the session was
	// idle) - result/error are this call's own.
	SteerRan SteerOutcome = iota
	// SteerEnqueued means the call was queued behind an active turn: a
	// call without a RunID will fold into that turn's next step, one
	// with a RunID runs recursively once the queue drains. result is
	// always nil.
	SteerEnqueued
	// SteerCanceled means the call was canceled on entry: a cancel
	// covering its accept sequence was already recorded before the
	// dispatch decision, so a canceled turn was persisted without ever
	// streaming. Only reachable when the call carries an Accepted
	// handle, which Steer callers submitting fresh interactive
	// follow-ups do not set.
	SteerCanceled
)

type SessionAgent interface {
	Run(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)
	// Steer submits call as an explicit steering follow-up: it makes the
	// same atomic enqueue-into-the-active-turn vs run-as-a-new-turn
	// decision Run makes as a side effect of its busy check, and reports
	// which one it took. See sessionAgent.run for the shared
	// implementation and sessionAgent.Steer for the contract.
	Steer(context.Context, SessionAgentCall) (SteerOutcome, *fantasy.AgentResult, error)
	BeginAccepted(sessionID string) *AcceptedRun
	SetModel(model Model)
	SetTools(tools []fantasy.AgentTool)
	SetSystemPrompt(systemPrompt string)
	Cancel(sessionID string)
	CancelAll()
	IsSessionBusy(sessionID string) bool
	IsBusy() bool
	QueuedPrompts(sessionID string) int
	QueuedPromptsList(sessionID string) []string
	ClearQueue(sessionID string)
	Summarize(context.Context, string, fantasy.ProviderOptions, func(context.Context, *fantasy.ProviderError) error) error
	Model() Model
	GenerateTitle(ctx context.Context, sessionID, userPrompt string)
	// DeliverTaskCompletion enqueues completion into sessionID's
	// completion inbox and, if the session is idle and was not left
	// canceled by the user, starts a continuation turn for it. See
	// dispatcher's completionInbox field, runTurn.prepareStep (the
	// mid-turn delivery path), and startContinuation (the wake path).
	DeliverTaskCompletion(ctx context.Context, sessionID string, completion TaskCompletion)
	// RegisterDelegationParent records where sessionID (a running
	// delegation's own child session) should deliver a mid-run ask via
	// SendToParent. See DelegationParent.
	RegisterDelegationParent(sessionID string, parent DelegationParent)
	// SendToParent delivers a mid-run ask from sessionID to its
	// registered parent (see RegisterDelegationParent), riding the same
	// completion-inbox delivery path DeliverTaskCompletion uses - at-
	// most-once, non-blocking, and folded into the parent's next step or
	// an idle-wake continuation. Returns an error, delivering nothing,
	// if sessionID has no registered parent.
	SendToParent(ctx context.Context, sessionID, message string) error
}

type Model struct {
	Model      fantasy.LanguageModel
	CatalogCfg catwalk.Model
	ModelCfg   config.SelectedModel
	FlatRate   bool
}

// activeCancel wraps a context.CancelFunc with a unique pointer identity.
// The pointer is used for compare-and-delete in the dispatch completion path:
// when a finishing run's deferred cleanup fires, it must only remove its own
// entry — not a newer run's entry that was installed in the window between
// the explicit Del and the function return.
type activeCancel struct {
	cancel context.CancelFunc
}

type sessionAgent struct {
	model              *csync.Value[Model]
	systemPromptPrefix *csync.Value[string]
	systemPrompt       *csync.Value[string]
	tools              *csync.Slice[fantasy.AgentTool]

	isSubAgent           bool
	sessions             session.Service
	messages             message.Service
	disableAutoSummarize bool
	notify               pubsub.Publisher[notify.Notification]
	runComplete          pubsub.Publisher[notify.RunComplete]
	mcp                  *mcp.Registry

	// dispatch owns the accept/queue/cancel protocol state shared by Run
	// and Summarize's dispatch handoffs. See dispatch.go.
	dispatch *dispatcher
}

type SessionAgentOptions struct {
	Model                Model
	SystemPromptPrefix   string
	SystemPrompt         string
	IsSubAgent           bool
	DisableAutoSummarize bool
	Sessions             session.Service
	Messages             message.Service
	Tools                []fantasy.AgentTool
	Notify               pubsub.Publisher[notify.Notification]
	RunComplete          pubsub.Publisher[notify.RunComplete]
	MCP                  *mcp.Registry
}

func NewSessionAgent(
	opts SessionAgentOptions,
) SessionAgent {
	// SessionAgentOptions builds an uncached runtime. Apply the fixed policy
	// before publishing its tools to any Run.
	if len(opts.Tools) > 0 {
		opts.Tools[len(opts.Tools)-1].SetProviderOptions(cacheControlOptions())
	}
	a := &sessionAgent{
		model:                csync.NewValue(opts.Model),
		systemPromptPrefix:   csync.NewValue(opts.SystemPromptPrefix),
		systemPrompt:         csync.NewValue(opts.SystemPrompt),
		isSubAgent:           opts.IsSubAgent,
		sessions:             opts.Sessions,
		messages:             opts.Messages,
		disableAutoSummarize: opts.DisableAutoSummarize,
		tools:                csync.NewSliceFrom(opts.Tools),
		notify:               opts.Notify,
		runComplete:          opts.RunComplete,
		mcp:                  opts.MCP,
		dispatch:             newDispatcher(),
	}
	// Wired after construction since the hook closes over a: dispatch
	// itself must stay free of any dependency on a or on pubsub (see
	// dispatcher's doc comment).
	a.dispatch.onQueueChanged = a.publishQueueChanged
	return a
}

// AcceptedRun owns exactly one accept reservation taken by
// BeginAccepted. It is the only carrier of accept-state across the
// backend.runAgent / Coordinator.Run / sessionAgent.Run layers: a
// counter > 0 means a dispatched prompt is in flight and has not yet
// completed the dispatch handoff in Run. Close is the only way to
// release the reservation and is idempotent.
//
// AcceptedRun and BeginAccepted/endAccepted live in dispatch.go as part of
// the dispatcher type.

// persistCanceledTurn writes the user/assistant records for a turn that
// was canceled before (or just as) streaming would have produced them.
// It creates the user message only when it was not already created by an
// earlier createUserMessage call (userMsgCreated) and this is not a
// continuation (whose Prompt is a placeholder, never persisted - see
// SessionAgentCall.Continuation - so there is never a user message to
// create here even on this fallback path), then writes an assistant
// message with FinishReasonCanceled. Both writes use
// context.WithoutCancel(ctx) so workspace shutdown (which cancels the run
// context) can't drop them.
func (a *sessionAgent) persistCanceledTurn(ctx context.Context, call SessionAgentCall, userMsgCreated bool) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if !userMsgCreated && !call.Continuation {
		if _, err := a.createUserMessage(writeCtx, call); err != nil {
			return err
		}
	}
	model := a.model.Get()
	assistant, err := a.messages.Create(writeCtx, call.SessionID, message.CreateMessageParams{
		Role:     message.Assistant,
		Parts:    []message.ContentPart{},
		Model:    model.ModelCfg.Model,
		Provider: model.ModelCfg.Provider,
	})
	if err != nil {
		return err
	}
	assistant.AddFinish(message.FinishReasonCanceled, "User canceled request", "")
	return a.messages.Update(writeCtx, assistant)
}

// publishRunComplete emits the authoritative terminal event for a turn.
// It honors the per-call OnComplete hook when set (so the coordinator can
// coalesce retries) and otherwise falls back to the RunComplete broker.
// ctx is used only for the bounded-blocking must-deliver publish; the
// terminal payload is supplied by the caller. This is the single emit path
// shared by the streaming defer and the cancel-on-entry early return so a
// caller waiting on RunComplete (e.g. `braid run` with a RunID) always
// observes exactly one terminal event regardless of which Run branch ends
// the turn.
func (a *sessionAgent) publishRunComplete(ctx context.Context, call SessionAgentCall, complete notify.RunComplete) {
	if call.OnComplete != nil {
		call.OnComplete(complete)
		return
	}
	if a.runComplete == nil {
		return
	}
	a.runComplete.PublishMustDeliver(ctx, pubsub.UpdatedEvent, complete)
}

// ValidateCall performs the cheap structural validation that
// sessionAgent.Run requires before a call can be dispatched: a call must
// carry either a non-empty prompt or a text attachment, and it must name a
// session. It is exported so callers that accept a run before dispatching it
// (e.g. backend.SendMessage) can apply the same checks and keep the error
// contract consistent.
func ValidateCall(call SessionAgentCall) error {
	if call.Prompt == "" && !message.ContainsTextAttachment(call.Attachments) {
		return ErrEmptyPrompt
	}
	if call.SessionID == "" {
		return ErrSessionMissing
	}
	return nil
}

// run is the shared implementation behind the exported Run and Steer:
// both dispatch through it so there is exactly one place that makes the
// cancel-on-entry / enqueue / become-active decision and exactly one
// per-session lock discipline guarding it. Run discards outcome; Steer
// reports it. See SteerOutcome.
func (a *sessionAgent) run(ctx context.Context, call SessionAgentCall) (outcome SteerOutcome, result *fantasy.AgentResult, retErr error) {
	if err := ValidateCall(call); err != nil {
		return SteerRan, nil, err
	}
	if call.Accepted != nil {
		call.acceptSeq = call.Accepted.seq
	}

	// genCtx/cancel are the run context and its cancel func, created under
	// the per-session dispatch mutex below so a concurrent Cancel can observe
	// the activeRequests entry before the assistant message exists.
	var (
		genCtx         context.Context
		cancel         context.CancelFunc
		userMsgCreated bool
	)

	// Serialize the dispatch decision (cancel-on-entry | queued | active)
	// against a concurrent Cancel. Cancel takes the same per-session lock, so
	// every cancel observes at least one of: a cancel mark, an activeRequests
	// entry, or a messageQueue entry it then clears. Holding the lock across
	// the busy check and the active registration also makes them atomic, so
	// two concurrent in-process callers — a burst of channel events, or a
	// channel event racing a typed prompt — cannot both pass the busy check
	// and start two runs on the same session.
	sessMu := a.dispatch.sessionMu(call.SessionID)
	sessMu.Lock()

	if call.Accepted != nil && a.dispatch.canceledBySeq(call.SessionID, call.Accepted.seq) {
		// Cancel-on-entry: a cancel arrived while this accepted run was
		// dispatched but not yet active, and this handle's accept sequence
		// is at or below the session's cancel mark. The mark is left in
		// place so sibling handles it also covers observe the same cancel;
		// release the accept reservation, drop the lock, and persist a
		// canceled turn without entering Stream.
		//
		// This path returns before the streaming defer that publishes
		// RunComplete is installed, so emit the terminal event explicitly.
		// Without it, a caller waiting on RunComplete for this RunID (e.g.
		// `braid run`, which ignores message events and blocks on
		// RunComplete) would hang on an immediately-canceled accepted run.
		call.Accepted.Close()
		sessMu.Unlock()
		reporter := newCompletionReporter(a, call)
		complete := notify.RunComplete{
			SessionID: call.SessionID,
			RunID:     call.RunID,
			Cancelled: true,
		}
		if err := a.persistCanceledTurn(ctx, call, false); err != nil {
			complete.Error = err.Error()
			reporter.publish(ctx, complete)
			return SteerCanceled, nil, err
		}
		reporter.publish(ctx, complete)
		return SteerCanceled, nil, nil
	}

	if a.IsSessionBusy(call.SessionID) {
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
			if call.Accepted != nil {
				call.Accepted.Close()
			}
			sessMu.Unlock()
			return SteerEnqueued, nil, nil
		}
		// Busy: an earlier prompt is active. Queue this call so it is
		// folded into (or sequenced after) the active turn, and release any
		// accept reservation. A Cancel arriving after this point sees the
		// active entry and clears the queue.
		//
		// enqueueCall strips OnComplete: the caller that supplied the hook
		// (typically coordinator.Run) has its own retry/coalesce scope that
		// ends when it returns, so by the time the queue drains nobody is
		// left to consume the buffered terminal event. The queued turn falls
		// back to the default broker publish, which is what existing
		// subscribers expect.
		a.enqueueCall(call)
		if call.Accepted != nil {
			call.Accepted.Close()
		}
		sessMu.Unlock()
		// enqueueCall itself must not call notifyQueueChanged (it runs
		// under sessMu, held by this caller, not by dispatch itself) - so
		// this calls it explicitly, only now that the lock is released.
		a.dispatch.notifyQueueChanged(call.SessionID)
		return SteerEnqueued, nil, nil
	}

	// Idle: become the active run. Register the cancel func before dropping
	// the lock so a Cancel that arrives between here and assistant creation
	// is not lost.
	runCtx := context.WithValue(ctx, tools.SessionIDContextKey, call.SessionID)
	genCtx, cancel = context.WithCancel(runCtx)
	ac := &activeCancel{cancel: cancel}
	a.dispatch.activeRequests.Set(call.SessionID, ac)
	// A new turn is genuinely starting for this session - whether a real
	// user Run/Steer or our own auto-continuation (see
	// enqueueCompletionAndDrainForWake) - so any earlier "user canceled
	// this session" marker is now stale.
	a.dispatch.cancelledSessions.Del(call.SessionID)
	if call.Accepted != nil {
		call.Accepted.Close()
	}
	sessMu.Unlock()

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
		csync.CompareAndDelete(a.dispatch.activeRequests, call.SessionID, ac)
		a.wakeFromInboxIfIdle(context.WithoutCancel(ctx), call.SessionID)
	}()

	// Copy mutable fields under lock to avoid races with SetTools/SetModel.
	// model is read exactly once here and used by value for the rest of the
	// turn (including queued continuations), so a concurrent SetModel cannot
	// change an in-flight run's identity.
	agentTools := a.tools.Copy()
	model := a.model.Get()
	if call.Runtime != nil {
		model = call.Runtime.model
		agentTools = slices.Clone(call.Runtime.tools)
	}
	systemPrompt := a.systemPrompt.Get()
	promptPrefix := a.systemPromptPrefix.Get()
	disableAutoSummarize := a.disableAutoSummarize
	if call.Runtime != nil {
		systemPrompt = call.Runtime.systemPrompt
		promptPrefix = call.Runtime.systemPromptPrefix
		disableAutoSummarize = call.Runtime.disableAutoSummarize
	}
	var instructions strings.Builder

	// a.mcp is nil for session agents built outside app.New (a handful of
	// tests construct one directly); treat that as "no MCP servers" rather
	// than panicking.
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
		systemPrompt += "\n\n<mcp-instructions>\n" + s + "\n</mcp-instructions>"
	}

	agent := fantasy.NewAgent(
		model.Model,
		fantasy.WithSystemPrompt(systemPrompt),
		fantasy.WithTools(agentTools...),
		fantasy.WithUserAgent(userAgent),
	)

	currentSession, err := a.sessions.Get(ctx, call.SessionID)
	if err != nil {
		return SteerRan, nil, fmt.Errorf("failed to get session: %w", err)
	}

	msgs, err := a.getSessionMessages(ctx, currentSession)
	if err != nil {
		return SteerRan, nil, fmt.Errorf("failed to get session messages: %w", err)
	}

	// Generate title from the first real (non-shell) user prompt.
	// can take tens of seconds. Blocking Run on it delays the
	// response to the caller. Use a detached context so the title
	// goroutine survives Run's cancel. Never for a continuation: its
	// Prompt is a placeholder (see continuationPromptPlaceholder), not
	// something to title a session from, and a continuation only ever
	// runs on a session that already has real conversation behind it.
	if !call.Continuation && !hasUserTextMessage(msgs) {
		titleCtx := context.WithoutCancel(ctx)
		go a.generateTitle(titleCtx, call.SessionID, call.Prompt, model, promptPrefix)
	}

	// Add the user message to the session - except for a continuation,
	// whose Prompt is a placeholder standing in for fantasy's own
	// non-empty-prompt requirement, not real content (see
	// continuationPromptPlaceholder and SessionAgentCall.Continuation).
	// Persisting it would put a message in history the user never wrote;
	// PrepareStep strips the placeholder from the model's own view of
	// this step and folds in the completion inbox instead, the same way
	// it already does for the mid-turn fold case.
	if !call.Continuation {
		_, err = a.createUserMessage(ctx, call)
		if err != nil {
			return SteerRan, nil, err
		}
		userMsgCreated = true
	}

	// Add the session to the context. The run context (genCtx) and its
	// cancel func were already created and registered under the dispatch
	// mutex above for both the accepted and in-process paths.
	ctx = context.WithValue(ctx, tools.SessionIDContextKey, call.SessionID)
	// reporter delivers this turn's terminal RunComplete exactly once,
	// regardless of whether it comes from the defer below (the
	// normal/error/panic-recovery catch-all) or from the explicit
	// publish on the queued-recursion tail path (which must win: it
	// carries this turn's own retErr before the recursive Run
	// clobbers the named return).
	reporter := newCompletionReporter(a, call)
	// t owns the streaming-callback state (currentAssistant, stepMessages,
	// currentSession, etc.) that PrepareStep and the other
	// fantasy.AgentStreamCall callbacks below read and write. It's
	// declared here so the deferred RunComplete publish below can read
	// t.currentAssistant, which PrepareStep will (re)assign once per
	// streaming step. The final assistant message of the turn is the
	// value reachable through that field when the defer runs.
	t := newRunTurn(a, call, ctx, genCtx, model, agentTools, promptPrefix, disableAutoSummarize, currentSession, userMsgCreated)
	// Drain any debounced message updates before returning. message.Service
	// already flushes synchronously on terminal updates, but a defer here
	// guarantees the contract at every Run exit (success, error, panic
	// recovery upstream) without callers needing to know.
	//
	// After the flush completes — meaning all per-message
	// Publish(UpdatedEvent) calls have fired and been buffered into
	// every subscriber's channel — publish the authoritative
	// RunComplete event for this turn. The flush-then-publish order
	// gives well-behaved clients the best chance of seeing the final
	// message event before RunComplete; the embedded Text field
	// reconciles for clients that observe the events out of order
	// (the pubsub broker fan-in does not serialize publishes from
	// different upstream brokers).
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
		if t.currentAssistant != nil {
			complete.MessageID = t.currentAssistant.ID
			complete.Text = t.currentAssistant.Content().String()
		}
		if retErr != nil {
			complete.Error = retErr.Error()
			complete.Cancelled = errors.Is(retErr, context.Canceled)
		} else if ctx.Err() != nil {
			complete.Cancelled = true
		}
		// Prefer the per-call hook when supplied so the coordinator
		// can coalesce retries (e.g. unauthorized → re-auth → retry)
		// into a single user-visible terminal event. The fallback
		// must-deliver publish applies bounded-blocking semantics to
		// the authoritative terminal event so a momentarily-full
		// subscriber channel can't silently drop it and hang
		// non-interactive clients waiting on RunComplete. reporter
		// ensures this is a no-op when the tail path below already
		// published this turn's RunComplete before recursing.
		reporter.publish(ctx, complete)
	}()

	// Carried-over history goes in front of this session's own messages
	// and is deliberately not folded into msgs above: msgs drives the
	// title decision and this session's summarize bookkeeping, neither
	// of which owns messages that belong to earlier sessions.
	history, files := a.preparePrompt(withPriorMessages(call.PriorMessages, msgs), model.CatalogCfg.SupportsImages, call.Attachments...)

	startTime := time.Now()
	a.eventPromptSent(call.SessionID)

	// Don't send MaxOutputTokens if 0 — some providers (e.g. LM Studio) reject it
	var maxOutputTokens *int64
	if call.MaxOutputTokens > 0 {
		maxOutputTokens = &call.MaxOutputTokens
	}
	result, err = agent.Stream(genCtx, fantasy.AgentStreamCall{
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
		OnRetry:          t.onRetry,
		OnAuthRefresh:    call.OnAuthRefresh,
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

	a.eventPromptResponded(call.SessionID, time.Since(startTime).Truncate(time.Second))

	if err != nil {
		streamResult, streamErr := t.handleStreamError(err)
		return SteerRan, streamResult, streamErr
	}

	if t.shouldSummarize {
		// Conditional release: only clear our own entry, mirroring the
		// defer above. An unconditional Del here would erase a newer
		// run's entry if one raced in and won the session in the window
		// this release opens up before Summarize does its own busy check.
		csync.CompareAndDelete(a.dispatch.activeRequests, call.SessionID, ac)
		if summarizeErr := a.summarize(genCtx, call.SessionID, call.ProviderOptions, call.OnAuthRefresh, model, promptPrefix, call.ActiveRuntime); summarizeErr != nil {
			return SteerRan, nil, summarizeErr
		}
		// If the agent wasn't done...
		if len(t.currentAssistant.ToolCalls()) > 0 {
			call.Prompt = fmt.Sprintf("The previous session was interrupted because it got too long, the initial user request was: `%s`", call.Prompt)
			a.dispatch.requeueContinuation(call, reporter.suppress)
		}
	}

	// Release active request before publishing the notification.
	// TUI handlers poll IsSessionBusy() and only re-evaluate when a
	// tea.Msg arrives, so the cleanup must precede the notify or
	// subscribers see stale busy state at the moment of receipt.
	//
	// Conditional release, like the two cleanups above: when shouldSummarize
	// already cleared our own entry (line ~1199), this is a no-op against
	// whatever newer run's entry (if any) is there now. When shouldSummarize
	// was false, ac is still our own entry here (nothing else clears it
	// first), so this behaves exactly like the unconditional Del it
	// replaces. Either way, our own entry can't leak: we only ever remove
	// it, never re-register it, after this point.
	csync.CompareAndDelete(a.dispatch.activeRequests, call.SessionID, ac)
	cancel()

	// Send notification that agent has finished its turn (skip for
	// nested/non-interactive sessions).
	if !call.NonInteractive && a.notify != nil {
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
	queuedMessages, firstQueued, canceledRunIDDrops := a.dispatch.drainNext(call.SessionID)
	// A dropped prompt carrying a RunID must still publish its terminal
	// cancelled RunComplete so a caller waiting on that RunID does not
	// hang.
	a.publishCanceledQueueDrops(canceledRunIDDrops)
	if firstQueued == nil {
		return SteerRan, result, err
	}
	// There are queued messages, restart the loop. Publishing this
	// turn's RunComplete explicitly below (when owed) fires reporter's
	// Once first, so the outer defer's later emit — which would
	// otherwise observe the recursive Run's retErr (named-return
	// clobbering through the return below) against this turn's
	// MessageID/Text — becomes a no-op instead of a mixed, racing
	// event.
	//
	// Decide whether this turn still owes its own terminal RunComplete.
	// Each submitted prompt with a RunID has its own lifecycle, so a turn
	// that is finished and handing off to a *different* queued prompt must
	// publish its own RunComplete here — leaving it to the recursive turn
	// (which carries a different RunID) would hang a caller waiting on
	// this turn's RunID. The exception is the summarize-continuation path,
	// which re-queues this same call (same RunID) to resume after a
	// summary; in that case the eventual terminal turn for this RunID
	// publishes, so publishing now would double-emit.
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
		if t.currentAssistant != nil {
			complete.MessageID = t.currentAssistant.ID
			complete.Text = t.currentAssistant.Content().String()
		}
		if ctx.Err() != nil {
			complete.Cancelled = true
		}
		reporter.publish(ctx, complete)
	} else {
		// Same-RunID re-queue: the recursive turn below owns this
		// RunID's terminal event. Spend reporter's Once now so the
		// streaming defer — which fires after the recursive Run
		// returns and would otherwise observe its clobbered retErr —
		// finds nothing left to publish.
		reporter.suppress()
	}
	// outcome is clobbered here exactly like result/retErr already are:
	// this call did run (it produced the turn above), but its return
	// value becomes whatever the recursively-dispatched *firstQueued
	// call's own dispatch decision was. A Steer caller only ever
	// observes this tail when its own call triggered an auto-summarize
	// continuation or handed off to something else queued behind it —
	// not on the plain busy/idle paths the busy- and idle-path tests
	// cover.
	return a.run(ctx, *firstQueued)
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

func (a *sessionAgent) createUserMessage(ctx context.Context, call SessionAgentCall) (message.Message, error) {
	parts := []message.ContentPart{message.TextContent{Text: call.Prompt}}
	var attachmentParts []message.ContentPart
	for _, attachment := range call.Attachments {
		attachmentParts = append(attachmentParts, message.BinaryContent{Path: attachment.FilePath, MIMEType: attachment.MimeType, Data: attachment.Content})
	}
	parts = append(parts, attachmentParts...)
	msg, err := a.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
		Role:   message.User,
		Parts:  parts,
		Origin: call.PromptOrigin,
	})
	if err != nil {
		return message.Message{}, fmt.Errorf("failed to create user message: %w", err)
	}
	return msg, nil
}

// withPriorMessages returns prior followed by own without writing into
// either backing array - msgs comes straight from the message store's
// slice and callers below still read it.
func withPriorMessages(prior, own []message.Message) []message.Message {
	if len(prior) == 0 {
		return own
	}
	combined := make([]message.Message, 0, len(prior)+len(own))
	combined = append(combined, prior...)
	combined = append(combined, own...)
	return combined
}

func (a *sessionAgent) SetModel(model Model) {
	a.model.Set(model)
}

func (a *sessionAgent) SetTools(tools []fantasy.AgentTool) {
	a.tools.SetSlice(tools)
}

func (a *sessionAgent) SetSystemPrompt(systemPrompt string) {
	a.systemPrompt.Set(systemPrompt)
}

func (a *sessionAgent) Model() Model {
	return a.model.Get()
}
