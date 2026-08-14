// Package agent is the core orchestration layer for Braid AI agents.
//
// It provides session-based AI agent functionality for managing
// conversations, tool execution, and message handling. It coordinates
// interactions between language models, messages, sessions, and tools while
// handling features like automatic summarization, queuing, and token
// management.
package agent

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	"github.com/rave-soft/braid/internal/agent/notify"
	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/agent/tools/mcp"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/stringext"
	"github.com/rave-soft/braid/internal/version"
)

const (
	DefaultSessionName = "Untitled Session"

	// Constants for auto-summarization thresholds
	largeContextWindowThreshold = 200_000
	largeContextWindowBuffer    = 20_000
	smallContextWindowRatio     = 0.2
)

var userAgent = fmt.Sprintf("Charm-Braid/%s (https://charm.land/braid)", version.Version)

//go:embed templates/title.md
var titlePrompt []byte

//go:embed templates/summary.md
var summaryPrompt []byte

// Used to remove <think> tags from generated titles.
var (
	thinkTagRegex       = regexp.MustCompile(`(?s)<think>.*?</think>`)
	orphanThinkTagRegex = regexp.MustCompile(`</?think>`)
)

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

// BeginAccepted increments the accept counter for sessionID and returns
// a handle whose Close is the only way to decrement it.
func (a *sessionAgent) BeginAccepted(sessionID string) *AcceptedRun {
	return a.dispatch.BeginAccepted(sessionID)
}

// enqueueCall appends call to the session's message queue. See
// dispatcher.enqueueCall.
func (a *sessionAgent) enqueueCall(call SessionAgentCall) {
	a.dispatch.enqueueCall(call)
}

// drainQueueForStep partitions the session's queued calls for the current
// streaming step. See dispatcher.drainQueueForStep.
func (a *sessionAgent) drainQueueForStep(sessionID string) (fold, canceledWithRunID []SessionAgentCall) {
	return a.dispatch.drainQueueForStep(sessionID)
}

// requeueDrained puts a suffix of a drainQueueForStep fold batch back onto
// the front of the session's queue. See dispatcher.requeueDrained.
func (a *sessionAgent) requeueDrained(sessionID string, remainder []SessionAgentCall) {
	a.dispatch.requeueDrained(sessionID, remainder)
}

// publishCanceledQueueDrops emits a terminal cancelled RunComplete for
// every dropped queued call that carries a RunID. A queued prompt removed
// from the queue without ever running — covered by a pending cancel, or
// cleared by Cancel/ClearQueue — would otherwise leave a caller blocked on
// that RunID: `braid run` ignores live message events and exits only on a
// RunComplete whose RunID matches. Calls without a RunID had no such waiter
// and are dropped silently as before. A detached, bounded context keeps the
// must-deliver publish alive even when the run context that triggered the
// drop is already canceled.
func (a *sessionAgent) publishCanceledQueueDrops(drops []SessionAgentCall) {
	var hasRunID bool
	for _, d := range drops {
		if d.RunID != "" {
			hasRunID = true
			break
		}
	}
	if !hasRunID {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, d := range drops {
		if d.RunID == "" {
			continue
		}
		newCompletionReporter(a, d).publish(ctx, notify.RunComplete{
			SessionID: d.SessionID,
			RunID:     d.RunID,
			Cancelled: true,
		})
	}
}

// clearPendingCancel removes any pending-cancel mark for sessionID. See
// dispatcher.clearPendingCancel.
func (a *sessionAgent) clearPendingCancel(sessionID string) {
	a.dispatch.clearPendingCancel(sessionID)
}

// DeliverTaskCompletion enqueues completion into sessionID's completion
// inbox and, if that leaves the session eligible (idle, not left
// canceled by the user), attempts a continuation turn for it. See
// dispatcher.enqueueCompletion and startContinuation. The attempt does
// not itself drain anything - the completion stays in the inbox until
// whichever turn actually becomes active drains it via PrepareStep, so a
// continuation call that loses its own busy-check race (see run()) drops
// cleanly without losing or duplicating completion's content.
func (a *sessionAgent) DeliverTaskCompletion(ctx context.Context, sessionID string, completion TaskCompletion) {
	if !a.dispatch.enqueueCompletion(sessionID, completion) {
		return
	}
	a.startContinuation(ctx, sessionID, "completion arrived while session was idle")
}

// RegisterDelegationParent records where sessionID should deliver a
// mid-run ask. See dispatcher.RegisterDelegationParent.
func (a *sessionAgent) RegisterDelegationParent(sessionID string, parent DelegationParent) {
	a.dispatch.RegisterDelegationParent(sessionID, parent)
}

// SendToParent delivers a mid-run ask from sessionID to its registered
// parent, riding the exact same delivery path DeliverTaskCompletion
// uses (enqueue, at-most-once, idle-wake) via the registered parent's
// own Coordinator - never this sessionAgent's, since a thread's parent
// may live in an entirely different Coordinator/App. Non-blocking: it
// enqueues and returns without waiting for a reply, exactly like
// DeliverTaskCompletion.
func (a *sessionAgent) SendToParent(ctx context.Context, sessionID, message string) error {
	parent, ok := a.dispatch.delegationParents.Get(sessionID)
	if !ok {
		return fmt.Errorf("agent: session %q has no registered parent to message", sessionID)
	}
	parent.Parent.DeliverTaskCompletion(ctx, parent.ParentSessionID, TaskCompletion{
		DelegationID:   parent.DelegationID,
		Kind:           parent.Kind,
		Name:           parent.Name,
		ChildSessionID: sessionID,
		Depth:          parent.Depth,
		TerminalAt:     time.Now(),
		IsMessage:      true,
		Message:        message,
	})
	return nil
}

// drainCompletionsForStep removes and returns every completion queued for
// sessionID. See dispatcher.drainCompletionsForStep.
func (a *sessionAgent) drainCompletionsForStep(sessionID string) []TaskCompletion {
	return a.dispatch.drainCompletionsForStep(sessionID)
}

// requeueCompletions puts a suffix of a drainCompletionsForStep batch back
// at the front of sessionID's completion inbox. See
// dispatcher.requeueCompletions.
func (a *sessionAgent) requeueCompletions(sessionID string, remainder []TaskCompletion) {
	a.dispatch.requeueCompletions(sessionID, remainder)
}

// publishQueueChanged is wired as dispatch.onQueueChanged (see
// NewSessionAgent) so the dispatcher can signal a queue mutation without
// importing pubsub itself. It publishes a lossy, best-effort
// notify.TypeQueueChanged event; the UI's queued-prompt pill re-probes
// the actual count off it instead of trusting a payload, and still falls
// back to its TTL if this is ever dropped by the broker. Called by
// dispatcher methods only after their per-session dispatch mutex has
// been released, so this may itself take a session's dispatch mutex (a
// subscriber callback could, in principle) without risk of deadlock.
func (a *sessionAgent) publishQueueChanged(sessionID string) {
	if a.notify == nil {
		return
	}
	a.notify.Publish(pubsub.CreatedEvent, notify.Notification{
		SessionID: sessionID,
		Type:      notify.TypeQueueChanged,
	})
}

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

func (a *sessionAgent) Summarize(ctx context.Context, sessionID string, opts fantasy.ProviderOptions, onAuthRefresh func(context.Context, *fantasy.ProviderError) error) error {
	return a.summarize(ctx, sessionID, opts, onAuthRefresh, a.model.Get(), a.systemPromptPrefix.Get(), nil)
}

func (a *sessionAgent) summarize(ctx context.Context, sessionID string, opts fantasy.ProviderOptions, onAuthRefresh func(context.Context, *fantasy.ProviderError) error, model Model, systemPromptPrefix string, active *activeRuntime) (retErr error) {
	sessMu := a.dispatch.sessionMu(sessionID)
	sessMu.Lock()
	if a.IsSessionBusy(sessionID) {
		sessMu.Unlock()
		return ErrSessionBusy
	}
	genCtx, cancel := context.WithCancel(ctx)
	ac := &activeCancel{cancel: cancel}
	a.dispatch.activeRequests.Set(sessionID, ac)
	sessMu.Unlock()

	defer func() {
		csync.CompareAndDelete(a.dispatch.activeRequests, sessionID, ac)
		cancel()

		_, next, canceledRunIDDrops := a.dispatch.drainNext(sessionID)
		a.publishCanceledQueueDrops(canceledRunIDDrops)
		if next == nil {
			return
		}
		_, handoffErr := a.Run(context.WithoutCancel(ctx), *next)
		if retErr == nil {
			retErr = handoffErr
		}
	}()

	currentSession, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	msgs, err := a.getSessionMessages(ctx, currentSession)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		// Nothing to summarize.
		return nil
	}

	aiMsgs, _ := a.preparePrompt(msgs, model.CatalogCfg.SupportsImages)

	defer func() {
		if flushErr := a.messages.FlushAll(ctx); flushErr != nil {
			slog.Error("Failed to flush pending message updates after summarize", "error", flushErr)
		}
	}()

	agent := fantasy.NewAgent(
		model.Model,
		fantasy.WithSystemPrompt(string(summaryPrompt)),
		fantasy.WithUserAgent(userAgent),
	)
	summaryMessage, err := a.messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:             message.Assistant,
		Model:            model.ModelCfg.Model,
		Provider:         model.ModelCfg.Provider,
		IsSummaryMessage: true,
	})
	if err != nil {
		return err
	}

	summaryPromptText := buildSummaryPrompt(currentSession.Todos)

	resp, err := agent.Stream(genCtx, fantasy.AgentStreamCall{
		Prompt:          summaryPromptText,
		Messages:        aiMsgs,
		Headers:         sessionHeaders(sessionID),
		ProviderOptions: opts,
		OnAuthRefresh:   onAuthRefresh,
		ModelProvider: func() fantasy.LanguageModel {
			if active != nil {
				if activeRuntime := active.load(); activeRuntime != nil && activeRuntime.model.ModelCfg.Provider == model.ModelCfg.Provider && activeRuntime.model.ModelCfg.Model == model.ModelCfg.Model {
					return activeRuntime.model.Model
				}
			}
			return model.Model
		},
		PrepareStep: func(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			prepared.Messages = options.Messages
			if systemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{fantasy.NewSystemMessage(systemPromptPrefix)}, prepared.Messages...)
			}
			return callContext, prepared, nil
		},
		OnReasoningDelta: func(id string, text string) error {
			summaryMessage.AppendReasoningContent(text)
			return a.messages.Update(genCtx, summaryMessage)
		},
		OnReasoningEnd: func(id string, reasoning fantasy.ReasoningContent) error {
			// Handle anthropic signature.
			if anthropicData, ok := reasoning.ProviderMetadata["anthropic"]; ok {
				if signature, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok && signature.Signature != "" {
					summaryMessage.AppendReasoningSignature(signature.Signature)
				}
			}
			summaryMessage.FinishThinking()
			return a.messages.Update(genCtx, summaryMessage)
		},
		OnTextDelta: func(id, text string) error {
			summaryMessage.AppendContent(text)
			return a.messages.Update(genCtx, summaryMessage)
		},
	})
	if err != nil {
		isCancelErr := errors.Is(err, context.Canceled)
		if isCancelErr {
			// User cancelled summarize we need to remove the summary message.
			deleteErr := a.messages.Delete(ctx, summaryMessage.ID)
			return deleteErr
		}
		// Mark the summary message as finished with an error so the UI
		// stops spinning.
		summaryMessage.AddFinish(message.FinishReasonError, "Summarization Error", err.Error())
		if updateErr := a.messages.Update(ctx, summaryMessage); updateErr != nil {
			return updateErr
		}
		return err
	}

	summaryMessage.AddFinish(message.FinishReasonEndTurn, "", "")
	err = a.messages.Update(genCtx, summaryMessage)
	if err != nil {
		return err
	}

	var openrouterCost *float64
	for _, step := range resp.Steps {
		stepCost := a.openrouterCost(step.ProviderMetadata)
		if stepCost != nil {
			newCost := *stepCost
			if openrouterCost != nil {
				newCost += *openrouterCost
			}
			openrouterCost = &newCost
		}
	}

	a.updateSessionUsage(model, &currentSession, resp.TotalUsage, openrouterCost, false)

	// Just in case, get just the last usage info.
	usage := resp.Response.Usage
	currentSession.SummaryMessageID = summaryMessage.ID
	currentSession.CompletionTokens = summaryCompletionTokens(usage, summaryMessage)
	currentSession.PromptTokens = 0
	currentSession.EstimatedUsage = usageIsZero(usage)
	_, err = a.sessions.Save(genCtx, currentSession)
	if err != nil {
		return err
	}

	return nil
}

func (a *sessionAgent) getCacheControlOptions() fantasy.ProviderOptions {
	return cacheControlOptions()
}

func cacheControlOptions() fantasy.ProviderOptions {
	if t, _ := strconv.ParseBool(os.Getenv("BRAID_DISABLE_ANTHROPIC_CACHE")); t {
		return fantasy.ProviderOptions{}
	}
	return fantasy.ProviderOptions{
		anthropic.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
		bedrock.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
		vercel.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
	}
}

// sessionHeaders returns the HTTP headers we use for cache affinity on
// every LLM request for a given session.
//
// We use the session hash is used instead of the raw UUID so the header
// value is deterministic and opaque.
func sessionHeaders(sessionID string) map[string]string {
	hash := session.HashID(sessionID)
	return map[string]string{
		"x-session-id":       hash,
		"x-session-affinity": hash,
	}
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

func (a *sessionAgent) preparePrompt(msgs []message.Message, supportsImages bool, attachments ...message.Attachment) ([]fantasy.Message, []fantasy.FilePart) {
	var history []fantasy.Message
	if !a.isSubAgent {
		history = append(history, fantasy.NewUserMessage(
			fmt.Sprintf(
				"<system_reminder>%s</system_reminder>",
				`This is a reminder that your todo list is currently empty. DO NOT mention this to the user explicitly because they are already aware.
If you are working on tasks that would benefit from a todo list please use the "todos" tool to create one.
If not, please feel free to ignore. Again do not mention this message to the user.`,
			),
		))
	}
	// Collect all tool call IDs present in assistant messages and all tool
	// result IDs present in tool messages. This lets us detect both orphaned
	// tool results (result without a call) and orphaned tool calls (call
	// without a result).
	knownToolCallIDs := make(map[string]struct{})
	knownToolResultIDs := make(map[string]struct{})
	for _, m := range msgs {
		switch m.Role {
		case message.Assistant:
			for _, tc := range m.ToolCalls() {
				knownToolCallIDs[tc.ID] = struct{}{}
			}
		case message.Tool:
			for _, tr := range m.ToolResults() {
				knownToolResultIDs[tr.ToolCallID] = struct{}{}
			}
		}
	}

	for _, m := range msgs {
		if len(m.Parts) == 0 {
			continue
		}
		// Assistant message without content or tool calls (cancelled before it returned anything).
		if m.Role == message.Assistant && len(m.ToolCalls()) == 0 && m.Content().Text == "" && m.ReasoningContent().String() == "" {
			continue
		}
		if m.Role == message.Tool {
			if msg, ok := filterOrphanedToolResults(m, knownToolCallIDs); ok {
				history = append(history, msg)
			}
			continue
		}
		aiMsgs := m.ToAIMessage()
		if !supportsImages {
			for i := range aiMsgs {
				if aiMsgs[i].Role == fantasy.MessageRoleUser {
					aiMsgs[i].Content = filterFileParts(aiMsgs[i].Content)
				}
			}
		}
		history = append(history, aiMsgs...)

		if m.Role == message.Assistant {
			if msg, ok := syntheticToolResultsForOrphanedCalls(m, knownToolResultIDs); ok {
				history = append(history, msg)
			}
		}
	}

	var files []fantasy.FilePart
	for _, attachment := range attachments {
		if attachment.IsText() {
			continue
		}
		if !supportsImages {
			continue
		}
		files = append(files, fantasy.FilePart{
			Filename:  attachment.FileName,
			Data:      attachment.Content,
			MediaType: attachment.MimeType,
		})
	}

	return history, files
}

// filterFileParts removes fantasy.FilePart entries from a slice of message
// parts. Used to strip image attachments from historical user messages when
// the current model does not support them.
func filterFileParts(parts []fantasy.MessagePart) []fantasy.MessagePart {
	filtered := make([]fantasy.MessagePart, 0, len(parts))
	for _, part := range parts {
		if _, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
			continue
		}
		filtered = append(filtered, part)
	}
	return filtered
}

// filterOrphanedToolResults converts a tool message to a fantasy.Message,
// dropping any tool result parts whose tool_call_id has no matching tool call
// in the known set. An orphaned result causes API validation to fail on every
// subsequent turn, permanently locking the session. Returns the filtered
// message and true if at least one valid part remains.
func filterOrphanedToolResults(m message.Message, knownToolCallIDs map[string]struct{}) (fantasy.Message, bool) {
	aiMsgs := m.ToAIMessage()
	if len(aiMsgs) == 0 {
		return fantasy.Message{}, false
	}
	var validParts []fantasy.MessagePart
	for _, part := range aiMsgs[0].Content {
		tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
		if !ok {
			validParts = append(validParts, part)
			continue
		}
		if _, known := knownToolCallIDs[tr.ToolCallID]; known {
			validParts = append(validParts, part)
		} else {
			slog.Warn(
				"Dropping orphaned tool result with no matching tool call",
				"tool_call_id", tr.ToolCallID,
			)
		}
	}
	if len(validParts) == 0 {
		return fantasy.Message{}, false
	}
	msg := aiMsgs[0]
	msg.Content = validParts
	return msg, true
}

// syntheticToolResultsForOrphanedCalls returns a tool message containing
// synthetic tool results for any tool calls in the assistant message that
// have no matching result in knownToolResultIDs. LLM APIs require every
// tool_use to be immediately followed by a tool_result; an interrupted
// session can leave orphaned tool_use blocks that permanently lock the
// conversation. Returns the message and true if any synthetic results were
// produced.
func syntheticToolResultsForOrphanedCalls(m message.Message, knownToolResultIDs map[string]struct{}) (fantasy.Message, bool) {
	var syntheticParts []fantasy.MessagePart
	for _, tc := range m.ToolCalls() {
		if _, hasResult := knownToolResultIDs[tc.ID]; hasResult {
			continue
		}
		slog.Warn(
			"Injecting synthetic tool result for orphaned tool call",
			"tool_call_id", tc.ID,
			"tool_name", tc.Name,
		)
		syntheticParts = append(syntheticParts, fantasy.ToolResultPart{
			ToolCallID: tc.ID,
			Output: fantasy.ToolResultOutputContentError{
				Error: errors.New("tool call was interrupted and did not produce a result, you may retry this call if the result is still needed"),
			},
		})
	}
	if len(syntheticParts) == 0 {
		return fantasy.Message{}, false
	}
	return fantasy.Message{
		Role:    fantasy.MessageRoleTool,
		Content: syntheticParts,
	}, true
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

func (a *sessionAgent) getSessionMessages(ctx context.Context, session session.Session) ([]message.Message, error) {
	msgs, err := a.messages.List(ctx, session.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to list messages: %w", err)
	}
	return trimToSummary(session, msgs), nil
}

// trimToSummary drops everything a session has already summarized away,
// leaving the summary itself as the leading user message. Shared by the
// live session's own history and by carried-over history from a named
// sub-agent's earlier sessions, so a summarized past turn is replayed
// the same way whichever side reads it.
func trimToSummary(session session.Session, msgs []message.Message) []message.Message {
	if session.SummaryMessageID == "" {
		return msgs
	}
	summaryMsgIndex := -1
	for i, msg := range msgs {
		if msg.ID == session.SummaryMessageID {
			summaryMsgIndex = i
			break
		}
	}
	if summaryMsgIndex == -1 {
		return msgs
	}
	msgs = msgs[summaryMsgIndex:]
	msgs[0].Role = message.User
	return msgs
}

// hasUserTextMessage reports whether msgs already contains a
// message.User message with text — either something the user typed
// themselves or a delegation's goal/follow-up dispatched with
// message.OriginAgent (Role is always message.User regardless of
// origin; see SessionAgentCall.PromptOrigin). Either way generateTitle
// already ran once for this session and must not run again and clobber
// that title with a shorter follow-up prompt.
func hasUserTextMessage(msgs []message.Message) bool {
	for _, msg := range msgs {
		if msg.Role != message.User {
			continue
		}
		for _, part := range msg.Parts {
			if tc, ok := part.(message.TextContent); ok && tc.Text != "" {
				return true
			}
		}
	}
	return false
}

// GenerateTitle generates a session title based on the initial prompt.
func (a *sessionAgent) GenerateTitle(ctx context.Context, sessionID string, userPrompt string) {
	a.generateTitle(ctx, sessionID, userPrompt, a.model.Get(), a.systemPromptPrefix.Get())
}

// generateTitle titles the session with a single call on model. There is no
// fallback between models: if the call fails, the deferred fallback below
// saves the default session name.
func (a *sessionAgent) generateTitle(ctx context.Context, sessionID string, userPrompt string, model Model, systemPromptPrefix string) {
	if userPrompt == "" {
		return
	}

	// Ensure the session always gets a title even if every path below
	// fails or the context is cancelled before we finish.
	var titleSaved bool
	defer func() {
		if !titleSaved {
			fallbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := a.sessions.Rename(fallbackCtx, sessionID, DefaultSessionName); err != nil {
				slog.Error("Failed to save fallback session title", "error", err)
			}
		}
	}()

	newAgent := func(m fantasy.LanguageModel, p []byte, tok int64) fantasy.Agent {
		return fantasy.NewAgent(
			m,
			fantasy.WithSystemPrompt(string(p)+"\n /no_think"),
			fantasy.WithMaxOutputTokens(tok),
			fantasy.WithUserAgent(userAgent),
		)
	}

	streamCall := fantasy.AgentStreamCall{
		Prompt:  fmt.Sprintf("Generate a concise title for the following content:\n\n%s\n <think>\n\n</think>", userPrompt),
		Headers: sessionHeaders(sessionID),
		PrepareStep: func(callCtx context.Context, opts fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			prepared.Messages = opts.Messages
			if systemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{
					fantasy.NewSystemMessage(systemPromptPrefix),
				}, prepared.Messages...)
			}
			return callCtx, prepared, nil
		},
	}

	tok := int64(40)
	if model.CatalogCfg.CanReason {
		tok = model.CatalogCfg.DefaultMaxTokens
	}
	agent := newAgent(model.Model, titlePrompt, tok)
	resp, err := agent.Stream(ctx, streamCall)
	if err != nil {
		// The deferred fallback will save the default session name.
		slog.Error("Failed to generate title", "err", err)
		return
	}
	if resp.Response.FinishReason == fantasy.FinishReasonLength {
		slog.Error("Title generation hit the token limit")
		return
	}

	// Clean up title.
	var title string
	title = strings.ReplaceAll(resp.Response.Content.Text(), "\n", " ")

	// Remove thinking tags if present.
	title = thinkTagRegex.ReplaceAllString(title, "")
	title = orphanThinkTagRegex.ReplaceAllString(title, "")

	title = strings.TrimSpace(title)
	if title == "" {
		// LLM returned empty content. Use the prompt itself as a
		// fallback title, truncated to 50 chars, before resorting to
		// the generic default.
		fallback := strings.ReplaceAll(userPrompt, "\n", " ")
		fallback = strings.TrimSpace(fallback)
		if len(fallback) > 50 {
			fallback = truncateRunes(fallback, 50)
		}
		title = cmp.Or(fallback, DefaultSessionName)
	}

	// Calculate usage and cost.
	var openrouterCost *float64
	for _, step := range resp.Steps {
		stepCost := a.openrouterCost(step.ProviderMetadata)
		if stepCost != nil {
			newCost := *stepCost
			if openrouterCost != nil {
				newCost += *openrouterCost
			}
			openrouterCost = &newCost
		}
	}

	modelConfig := model.CatalogCfg
	cost := modelConfig.CostPer1MInCached/1e6*float64(resp.TotalUsage.CacheCreationTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(resp.TotalUsage.CacheReadTokens) +
		modelConfig.CostPer1MIn/1e6*float64(resp.TotalUsage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(resp.TotalUsage.OutputTokens)

	// Use override cost if available (e.g., from OpenRouter).
	if openrouterCost != nil {
		cost = *openrouterCost
	}

	// Skip cost accumulation
	if model.FlatRate {
		cost = 0
	}

	promptTokens := resp.TotalUsage.InputTokens + resp.TotalUsage.CacheCreationTokens
	completionTokens := resp.TotalUsage.OutputTokens

	// Atomically update only title and usage fields to avoid overriding other
	// concurrent session updates.
	saveErr := a.sessions.UpdateTitleAndUsage(ctx, sessionID, title, promptTokens, completionTokens, cost)
	if saveErr != nil {
		slog.Error("Failed to save session title and usage", "error", saveErr)
		return
	}
	titleSaved = true
}

// truncateRunes truncates s to at most n runes, appending "…" when
// truncation occurred. It's a dependency-free stand-in for ANSI-width-aware
// truncation, good enough for a plain-text session-title fallback.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (a *sessionAgent) openrouterCost(metadata fantasy.ProviderMetadata) *float64 {
	openrouterMetadata, ok := metadata[openrouter.Name]
	if !ok {
		return nil
	}

	opts, ok := openrouterMetadata.(*openrouter.ProviderMetadata)
	if !ok {
		return nil
	}
	return &opts.Usage.Cost
}

func (a *sessionAgent) updateSessionUsage(model Model, session *session.Session, usage fantasy.Usage, overrideCost *float64, estimated bool) {
	if !usageIsZero(usage) {
		session.EstimatedUsage = estimated
	}

	modelConfig := model.CatalogCfg
	cost := modelConfig.CostPer1MInCached/1e6*float64(usage.CacheCreationTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(usage.CacheReadTokens) +
		modelConfig.CostPer1MIn/1e6*float64(usage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(usage.OutputTokens)

	if !estimated {
		a.eventTokensUsed(session.ID, model, usage, cost)
	}

	if estimated {
		cost = 0
	} else {
		// Use override cost if available (e.g., from OpenRouter).
		if overrideCost != nil {
			cost = *overrideCost
		}

		// Skip cost accumulation
		if model.FlatRate {
			cost = 0
		}
	}

	session.Cost += cost
	updateSessionTokenCounters(session, usage)
}

func updateSessionTokenCounters(session *session.Session, usage fantasy.Usage) {
	if usage.OutputTokens != 0 {
		session.CompletionTokens = usage.OutputTokens
	}
	if promptTokens := usage.InputTokens + usage.CacheReadTokens; promptTokens != 0 {
		session.PromptTokens = promptTokens
	}
}

func summaryCompletionTokens(usage fantasy.Usage, summaryMessage message.Message) int64 {
	if usage.OutputTokens != 0 {
		return usage.OutputTokens
	}
	return approxTokenCount(summaryMessage.Content().Text) + approxTokenCount(summaryMessage.ReasoningContent().String())
}

// Cancel cancels sessionID's active run (if any) and any accepted or
// queued follow-ups. See dispatcher.cancel.
func (a *sessionAgent) Cancel(sessionID string) {
	drops := a.dispatch.cancel(sessionID)
	a.publishCanceledQueueDrops(drops)
}

// ClearQueue drops sessionID's queued follow-ups without touching the
// active run or any pending cancel mark. See dispatcher.clearQueue.
func (a *sessionAgent) ClearQueue(sessionID string) {
	drops := a.dispatch.clearQueue(sessionID)
	a.publishCanceledQueueDrops(drops)
}

func (a *sessionAgent) CancelAll() {
	if !a.IsBusy() {
		return
	}
	for _, sessionID := range a.dispatch.activeSessionIDs() {
		a.Cancel(sessionID)
	}

	// Poll IsBusy on a short tick until every canceled run's cleanup has
	// run, bounded by the same 5s budget as before. activeRequests has no
	// broadcast/wait primitive (csync.Map is a plain generic map used well
	// beyond this file), so a true event-driven wait would mean adding one
	// to a shared type for a single caller. A 10ms tick keeps this from
	// being a coarse 200ms busy-wait while staying a minimal, local change.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for a.IsBusy() {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *sessionAgent) IsBusy() bool {
	return a.dispatch.IsBusy()
}

func (a *sessionAgent) IsSessionBusy(sessionID string) bool {
	return a.dispatch.IsSessionBusy(sessionID)
}

func (a *sessionAgent) QueuedPrompts(sessionID string) int {
	return a.dispatch.QueuedPrompts(sessionID)
}

func (a *sessionAgent) QueuedPromptsList(sessionID string) []string {
	return a.dispatch.QueuedPromptsList(sessionID)
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

// convertToToolResult converts a fantasy tool result to a message tool result.
func (a *sessionAgent) convertToToolResult(result fantasy.ToolResultContent) message.ToolResult {
	baseResult := message.ToolResult{
		ToolCallID: result.ToolCallID,
		Name:       result.ToolName,
		Metadata:   result.ClientMetadata,
	}

	switch result.Result.GetType() {
	case fantasy.ToolResultContentTypeText:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result.Result); ok {
			baseResult.Content = r.Text
		}
	case fantasy.ToolResultContentTypeError:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result.Result); ok {
			baseResult.Content = r.Error.Error()
			baseResult.IsError = true
		}
	case fantasy.ToolResultContentTypeMedia:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](result.Result); ok {
			if !stringext.IsValidBase64(r.Data) {
				slog.Warn(
					"Tool returned media with invalid base64 data, discarding image",
					"tool", result.ToolName,
					"tool_call_id", result.ToolCallID,
				)
				baseResult.Content = "Tool returned image data with invalid encoding"
				baseResult.IsError = true
			} else {
				content := r.Text
				if content == "" {
					content = fmt.Sprintf("Loaded %s content", r.MediaType)
				}
				baseResult.Content = content
				baseResult.Data = r.Data
				baseResult.MIMEType = r.MediaType
			}
		}
	}

	return baseResult
}

// workaroundProviderMediaLimitations converts media content in tool results to
// user messages for providers that don't natively support images in tool results.
//
// Problem: OpenAI, Google, OpenRouter, and other OpenAI-compatible providers
// don't support sending images/media in tool result messages - they only accept
// text in tool results. However, they DO support images in user messages.
//
// If we send media in tool results to these providers, the API returns an error.
//
// Solution: For these providers, we:
//  1. Replace the media in the tool result with a text placeholder
//  2. Inject a user message immediately after with the image as a file attachment
//  3. This maintains the tool execution flow while working around API limitations
//
// Anthropic and Bedrock support images natively in tool results, so we skip
// this workaround for them.
//
// Example transformation:
//
//	BEFORE: [tool result: image data]
//	AFTER:  [tool result: "Image loaded - see attached"], [user: image attachment]
func (a *sessionAgent) workaroundProviderMediaLimitations(messages []fantasy.Message, model Model) []fantasy.Message {
	providerSupportsMedia := model.ModelCfg.Provider == string(catwalk.InferenceProviderAnthropic) ||
		model.ModelCfg.Provider == string(catwalk.InferenceProviderBedrock) ||
		model.ModelCfg.Provider == string(catwalk.InferenceProviderBedrockEurope)

	if providerSupportsMedia {
		return messages
	}

	supportsImages := model.CatalogCfg.SupportsImages

	convertedMessages := make([]fantasy.Message, 0, len(messages))

	for _, msg := range messages {
		if msg.Role != fantasy.MessageRoleTool {
			convertedMessages = append(convertedMessages, msg)
			continue
		}

		textParts := make([]fantasy.MessagePart, 0, len(msg.Content))
		var mediaFiles []fantasy.FilePart

		for _, part := range msg.Content {
			toolResult, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				textParts = append(textParts, part)
				continue
			}

			if media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](toolResult.Output); ok {
				if !supportsImages {
					// Model cannot process images. Replace with a text
					// placeholder and skip creating a synthetic user
					// message with FilePart, which would brick the
					// session on text-only models.
					textParts = append(textParts, fantasy.ToolResultPart{
						ToolCallID: toolResult.ToolCallID,
						Output: fantasy.ToolResultOutputContentText{
							Text: "[Image/media content not supported by this model]",
						},
						ProviderOptions: toolResult.ProviderOptions,
					})
					continue
				}

				decoded, err := base64.StdEncoding.DecodeString(media.Data)
				if err != nil {
					slog.Warn("Failed to decode media data", "error", err)
					textParts = append(textParts, part)
					continue
				}

				mediaFiles = append(mediaFiles, fantasy.FilePart{
					Data:      decoded,
					MediaType: media.MediaType,
					Filename:  fmt.Sprintf("tool-result-%s", toolResult.ToolCallID),
				})

				textParts = append(textParts, fantasy.ToolResultPart{
					ToolCallID: toolResult.ToolCallID,
					Output: fantasy.ToolResultOutputContentText{
						Text: "[Image/media content loaded - see attached file]",
					},
					ProviderOptions: toolResult.ProviderOptions,
				})
			} else {
				textParts = append(textParts, part)
			}
		}

		convertedMessages = append(convertedMessages, fantasy.Message{
			Role:    fantasy.MessageRoleTool,
			Content: textParts,
		})

		if len(mediaFiles) > 0 {
			convertedMessages = append(convertedMessages, fantasy.NewUserMessage(
				"Here is the media content from the tool result:",
				mediaFiles...,
			))
		}
	}

	return convertedMessages
}

// buildSummaryPrompt constructs the prompt text for session summarization.
func buildSummaryPrompt(todos []session.Todo) string {
	var sb strings.Builder
	sb.WriteString("Provide a detailed summary of our conversation above.")
	if len(todos) > 0 {
		sb.WriteString("\n\n## Current Todo List\n\n")
		for _, t := range todos {
			fmt.Fprintf(&sb, "- [%s] %s\n", t.Status, t.Content)
		}
		sb.WriteString("\nInclude these tasks and their statuses in your summary. ")
		sb.WriteString("Instruct the resuming assistant to use the `todos` tool to continue tracking progress on these tasks.")
	}
	return sb.String()
}

func providerRetryLogFields(err *fantasy.ProviderError, delay time.Duration) []any {
	fields := []any{
		"retry_delay", delay.String(),
	}
	if err == nil {
		return fields
	}
	fields = append(fields, "status_code", err.StatusCode)
	if err.Title != "" {
		fields = append(fields, "title", err.Title)
	}
	if err.Message != "" {
		fields = append(fields, "message", err.Message)
	}
	return fields
}

// sanitizeToolInput validates tool call JSON from the provider.
// Malformed input is replaced with an empty object to prevent
// stuck conversations from truncated or malformed model output.
// The second return value indicates whether sanitization occurred.
func sanitizeToolInput(toolName, toolCallID, input string) (string, bool) {
	if !json.Valid([]byte(input)) {
		slog.Warn(
			"Malformed tool call JSON from provider, replacing with empty object",
			"tool", toolName,
			"id", toolCallID,
			"input_len", len(input),
		)
		return "{}", true
	}
	return input, false
}
