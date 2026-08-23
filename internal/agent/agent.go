// Package agent is the core orchestration layer for Sennit AI agents.
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
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/latency"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/version"
)

var userAgent = fmt.Sprintf(brand.Name+"/%s ("+brand.RepoURL+")", version.Version)

type SessionAgentCall struct {
	SessionID string
	// RunID, when non-empty, is the caller-supplied correlator that
	// gets echoed back on the notify.RunComplete event emitted for
	// this turn. It is preserved when the call is enqueued behind a
	// busy session so the queued turn's terminal event is still
	// recognisable to the original caller. Callers that need a
	// reliable completion contract (e.g. `sennit run` against a
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
	// toAIMessage in message_convert.go).
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
	// event, so non-interactive clients (e.g. `sennit run`) don't
	// exit on a stale failed-attempt RunComplete before the
	// successful retry. It is intentionally stripped when queueing
	// a busy-session call (see Run): the originating
	// coordinator.Run has long returned by the time the queued
	// recursion drains, so falling back to the default broker
	// publish keeps the event visible to subscribers.
	OnComplete func(notify.RunComplete)
	// Accepted, when non-nil, is the accept reservation taken by
	// BeginAccepted before the call was dispatched onto a goroutine
	// (the fire-and-forget dispatch path). Run consumes it under
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
	// Steering marks a follow-up the person typed at a session that may
	// already be mid-turn, asking for it to reach that turn rather than
	// wait for it: when run's dispatch decision finds the session busy,
	// this call's RunID is dropped as it is enqueued, so
	// drainQueueForStep folds it into the active turn's next step instead
	// of sequencing a separate turn behind it (see [SteerOutcome]).
	//
	// The RunID is kept — and the call runs as its own turn under it —
	// when the session turns out to be idle, which is why a steering
	// caller supplies one at all: it cannot know which of the two will
	// happen, and only the idle branch produces a turn whose terminal
	// RunComplete anyone can correlate. OnDispatch reports which branch
	// was taken, under the same lock that took it.
	Steering bool
	// OnDispatch, when non-nil, is called exactly once with the dispatch
	// decision this call reached, from inside the per-session dispatch
	// mutex that made it and before the turn (if any) starts streaming.
	//
	// It exists for callers whose own bookkeeping has to branch on the
	// decision at the moment it is made rather than when the turn ends:
	// internal/thread hands workspace ownership to a new run only if one
	// actually started, and observing that after the fact would race the
	// in-flight turn's completion. Callers that only care about the
	// result should use Steer, which reports the same outcome on return.
	//
	// The hook must not block and must not take the dispatch mutex: it
	// runs under it.
	OnDispatch func(SteerOutcome)
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
	// with a RunID runs recursively once the queue drains (a
	// SessionAgentCall.Steering call has its RunID dropped on the way
	// into the queue, so it always takes the folding side). result is
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

	// latency is nil-safe: when nil, the two handoff waits are still
	// logged, just not recorded. Every test that builds an agent without
	// a database relies on that, and measurement must never be the
	// reason a turn cannot run.
	latency latency.Recorder

	// dispatch owns the accept/queue/cancel protocol state shared by Run
	// and Summarize's dispatch handoffs. Embedded (via the dispatch
	// alias) so dispatcher's pure pass-through methods are promoted onto
	// SessionAgent's method set without a forwarding wrapper for each -
	// see dispatch.go.
	*dispatch
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
	// Latency is optional; see sessionAgent.latency.
	Latency latency.Recorder
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
		latency:              opts.Latency,
		dispatch:             newDispatcher(),
	}
	// Wired after construction since the hook closes over a: dispatch
	// itself must stay free of any dependency on a or on pubsub (see
	// dispatcher's doc comment).
	a.onQueueChanged = a.publishQueueChanged
	return a
}

// AcceptedRun owns exactly one accept reservation taken by
// BeginAccepted. It is the only carrier of accept-state across the
// AgentDispatcher.run / Coordinator.Run / sessionAgent.Run layers: a
// counter > 0 means a dispatched prompt is in flight and has not yet
// completed the dispatch handoff in Run. Close is the only way to
// release the reservation and is idempotent.
//
// AcceptedRun and BeginAccepted/endAccepted live in dispatch.go as part of
// the dispatcher type.

// recordLatency records one observed handoff wait, if a recorder was
// wired. It exists so the two call sites in turn.go can stay a single
// line each and need no nil check of their own.
func (a *sessionAgent) recordLatency(ctx context.Context, kind latency.Kind, sessionID string, waited time.Duration) {
	if a.latency == nil {
		return
	}
	a.latency.Record(ctx, kind, sessionID, waited)
}

// publishRunComplete emits the authoritative terminal event for a turn.
// It honors the per-call OnComplete hook when set (so the coordinator can
// coalesce retries) and otherwise falls back to the RunComplete broker.
// ctx is used only for the bounded-blocking must-deliver publish; the
// terminal payload is supplied by the caller. This is the single emit path
// shared by the streaming defer and the cancel-on-entry early return so a
// caller waiting on RunComplete (e.g. `sennit run` with a RunID) always
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
// (e.g. AgentDispatcher.Send) can apply the same checks and keep the error
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
	if call.Accepted != nil {
		call.acceptSeq = call.Accepted.seq
	}

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

	if call.Accepted != nil && a.canceledBySeq(s, call.Accepted.seq) {
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
			dispatched(SteerEnqueued)
			s.mu.Unlock()
			return dispatchOutcome{steer: SteerEnqueued}
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
		// thread_wait, say). The prompt stays queued either way; this
		// just stops the turn from sitting idle on something else while
		// the user has already moved on.
		a.signalUserInput(call.SessionID)
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

// buildStreamAgent assembles this turn's snapshot of model, tools and
// system prompt — from call.Runtime when a coordinator has pinned one, or
// from the agent's own mutable fields otherwise — appends any connected MCP
// server's instructions, and constructs the fantasy.Agent Stream runs on.
//
// Mutable fields are copied here, once, under their own locks (not the
// dispatch mutex): model is read exactly once and used by value for the
// rest of the turn, including queued continuations, so a concurrent
// SetModel cannot change an in-flight run's identity.
func (a *sessionAgent) buildStreamAgent(call SessionAgentCall) (streamAgent fantasy.Agent, model Model, agentTools []fantasy.AgentTool, promptPrefix string, disableAutoSummarize bool) {
	agentTools = a.tools.Copy()
	model = a.model.Get()
	if call.Runtime != nil {
		model = call.Runtime.model
		agentTools = slices.Clone(call.Runtime.tools)
	}
	agentTools = withoutUnusableParentTool(agentTools, a.dispatch, call.SessionID)
	systemPrompt := a.systemPrompt.Get()
	promptPrefix = a.systemPromptPrefix.Get()
	disableAutoSummarize = a.disableAutoSummarize
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

	streamAgent = fantasy.NewAgent(
		model.Model,
		fantasy.WithSystemPrompt(systemPrompt),
		fantasy.WithTools(agentTools...),
		fantasy.WithUserAgent(userAgent),
	)
	return streamAgent, model, agentTools, promptPrefix, disableAutoSummarize
}

// finishTurn concludes a turn whose Stream call returned without error: it
// runs auto-summarize if the turn tripped the context-window threshold,
// releases this turn's active-request slot, fires the finished notification,
// and hands off to the next queued call, if any.
//
// It is runTurn's only source of a non-nil next call, which is what lets
// run's loop replace the old tail recursion: runTurn returns next unchanged
// to its caller, which loops instead of calling itself.
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
	if t.shouldSummarize {
		// Conditional release: only clear our own entry, mirroring
		// runTurn's own cleanup defer. An unconditional Del here would
		// erase a newer run's entry if one raced in and won the session
		// in the window this release opens up before Summarize does its
		// own busy check.
		a.clearActiveIfMatch(call.SessionID, ac)
		if summarizeErr := a.summarize(genCtx, call.SessionID, call.ProviderOptions, call.OnAuthRefresh, t.model, t.promptPrefix, call.ActiveRuntime); summarizeErr != nil {
			return nil, nil, summarizeErr
		}
		// If the agent wasn't done...
		if len(t.currentAssistant.ToolCalls()) > 0 {
			call.Prompt = fmt.Sprintf("The previous session was interrupted because it got too long, the initial user request was: `%s`", call.Prompt)
			a.requeueContinuation(call, reporter.suppress)
		}
	}

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
	queuedMessages, firstQueued, canceledRunIDDrops := a.drainNext(call.SessionID)
	// A dropped prompt carrying a RunID must still publish its terminal
	// cancelled RunComplete so a caller waiting on that RunID does not
	// hang.
	a.publishCanceledQueueDrops(canceledRunIDDrops)
	if firstQueued == nil {
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
	// Best-effort on purpose: a session whose model failed to persist runs
	// exactly as it did before this was recorded at all, and failing a
	// turn over its bookkeeping would be the worse trade.
	a.recordSessionModel(ctx, call.SessionID)

	// reporter is this turn's single owner of the terminal RunComplete
	// event (see completionReporter's own doc comment). It is
	// constructed here, the moment this call has genuinely become the
	// active run, rather than after the fallible session/message setup
	// below - a session.Get, getSessionMessages, or createUserMessage
	// failure used to return before any reporter existed at all, which
	// silently discarded the prompt: nothing persisted, and no terminal
	// event for a caller waiting on one. See the fallback defer below.
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
	// message.Service already flushes synchronously on terminal updates;
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
	// a failure in any of those used to return before any such defer
	// existed at all, silently discarding the prompt - nothing
	// persisted, and no terminal event for a caller waiting on one.
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

	streamAgent, model, agentTools, promptPrefix, disableAutoSummarize := a.buildStreamAgent(call)

	currentSession, err := a.sessions.Get(ctx, call.SessionID)
	if err != nil {
		return SteerRan, nil, nil, fmt.Errorf("failed to get session: %w", err)
	}

	msgs, err := a.getSessionMessages(ctx, currentSession)
	if err != nil {
		return SteerRan, nil, nil, fmt.Errorf("failed to get session messages: %w", err)
	}

	if !call.Continuation && !hasUserTextMessage(msgs) {
		titleCtx := context.WithoutCancel(ctx)
		go a.generateTitle(titleCtx, call.SessionID, call.Prompt, model, promptPrefix)
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
	t = newRunTurn(a, call, ctx, genCtx, model, agentTools, promptPrefix, disableAutoSummarize, currentSession, userMsgCreated)

	// Carried-over history goes in front of this session's own
	// messages.
	history, files := a.preparePrompt(withPriorMessages(call.PriorMessages, msgs), model.CatalogCfg.SupportsImages, call.Attachments...)

	// Only this session's own messages, not the carried ones: a summary
	// replaces exactly what msgs holds, which is what stopOnContextWindow
	// needs to know to tell a summarizable context from an irreducible
	// one.
	ownHistory, _ := a.preparePrompt(msgs, model.CatalogCfg.SupportsImages)
	t.historyTokens = estimateMessageTokens(ownHistory)

	startTime := time.Now()
	a.eventPromptSent(call.SessionID)

	// Don't send MaxOutputTokens if 0 — some providers (e.g. LM Studio) reject it
	var maxOutputTokens *int64
	if call.MaxOutputTokens > 0 {
		maxOutputTokens = &call.MaxOutputTokens
	}
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
		return SteerRan, streamResult, nil, streamErr
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
// The loop replaces what used to be tail recursion — runTurn's finishTurn
// tail handing off to a queued call by calling run again. Literal recursion
// here had two problems: the recursive call's return values landed in this
// function's own named returns before the deferred publish ran (worked
// around by completionReporter's Once, not by the return values being
// right), and an arbitrarily long queue behind a busy session grew the
// goroutine stack by one frame per hop. Looping removes both: each call to
// runTurn is a self-contained function invocation with its own named
// returns and its own defers, so there is nothing left for a later hop to
// clobber, and the stack no longer grows with the queue's length.
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

// recordSessionModel pins sessionID to the model this agent is currently
// running, so a later restore of that session can put the model back. See
// session.Session.Model.
//
// Failures are logged and swallowed: this is bookkeeping alongside a turn
// that is already starting, and a session that keeps no model behaves the
// way every session behaved before the column existed — it falls back to
// the instance's own selection.
func (a *sessionAgent) recordSessionModel(ctx context.Context, sessionID string) {
	if a.sessions == nil {
		return
	}
	cfg := a.model.Get().ModelCfg
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

func (a *sessionAgent) SetTools(tools []fantasy.AgentTool) {
	a.tools.SetSlice(tools)
}

func (a *sessionAgent) SetSystemPrompt(systemPrompt string) {
	a.systemPrompt.Set(systemPrompt)
}

func (a *sessionAgent) Model() Model {
	return a.model.Get()
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
