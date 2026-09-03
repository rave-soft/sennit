package agent

import (
	"context"
	"fmt"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/message"
)

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
	// OnRateLimit, when non-nil, is called by fantasy when a stream fails
	// with a 429 (rate limit) response, mirroring OnAuthRefresh but for
	// the reactive rotation trigger: the
	// callback marks the exhausted account cooling down, picks another
	// via the provider's Rotator, and applies it. Returning nil retries
	// immediately with the new account's credentials; returning an error
	// (accounts.ErrAllExhausted) surfaces the original 429 unchanged. See
	// runtimeBuilder.makeRateLimitCallback. When the call carries no
	// rotation callback (rotation disabled, or a RotateThreshold
	// provider), this stays nil and fantasy never engages the hook.
	OnRateLimit func(ctx context.Context, err *fantasy.ProviderError) error
	// RotateThreshold, when non-nil, is called once per finished step
	// (runTurn.onStepFinish) for the proactive rotation trigger (Codex
	// today): it checks the active account's usage snapshot and
	// switches to another account if it has crossed the configured
	// threshold. It never fails the turn - errors are logged and
	// swallowed internally, exactly like today's no-rotation behavior
	// when a request happens to run over quota. nil when rotation is
	// disabled or the provider isn't a RotateThreshold one.
	RotateThreshold func(ctx context.Context)
	// Runtime carries the model and tools selected by the coordinator for
	// this lifecycle. It is preserved across queued continuations.
	Runtime       *compiledRuntime
	ActiveRuntime *activeRuntime
	// streamRuntime is an internal, immutable per-run snapshot. Delegations
	// create it after readiness and use it for both carry budgeting and Stream.
	streamRuntime *streamRuntime
	// Depth is how many delegation levels below a person's own turn this
	// turn runs: 0 for a session someone drives directly (the zero value,
	// so every ordinary caller gets it for free), 1 for a delegation that
	// session started, 2 for a delegation started by *that* delegation.
	// A Continuation call starts at 0 too and PrepareStep sets it from
	// what it drained: the completions report the depth of the
	// delegations that produced them, and reacting to your own
	// delegation's result puts you back at your own level, not a deeper
	// one - see runTurn.foldCompletions. Either way, PrepareStep stamps
	// the final value onto the step's context (tools.DepthContextKey) so
	// the delegation tools can refuse to nest past maxTaskCascadeDepth.
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
	// SteerDropped means the call was discarded outright rather than run,
	// queued, or canceled: a continuation call found another turn already
	// active and was dropped instead of enqueued, because its placeholder
	// prompt would otherwise be persisted as if it were real content (see
	// the drop site in dispatchDecision). result is always nil.
	SteerDropped
)

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
