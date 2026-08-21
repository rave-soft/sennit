package agent

import (
	"context"
	"fmt"
	"log/slog"

	"charm.land/fantasy"
)

// maxTaskCascadeDepth bounds how many auto-woken continuations may chain
// before further background work is refused: a user turn is depth 0; a
// task it creates inherits depth 0; the continuation that task's
// completion wakes runs at depth 1; a task *that* continuation creates
// inherits depth 1; its completion wakes a depth-2 continuation; and so
// on. At depth 3 a continuation still runs (it still has real work - the
// completion that woke it - to react to), but the "agent" tool's
// background mode refuses to start a new task from it: without a bound,
// a task that finishes and starts another task loops forever with no
// human in it. This is a hard constant, not configurable — the failure
// mode it guards against (an unattended cascade quietly burning tokens)
// is not something a misconfigured value should be able to reopen.
const maxTaskCascadeDepth = 3

// continuationPromptPlaceholder is the fantasy.Call.Prompt an auto-woken
// continuation carries. It exists purely to satisfy fantasy's own
// createPrompt validation, which refuses an empty prompt unless the
// session's prior history already ends in a user or tool message - not
// the case for a freshly-idle session whose last turn ended in an
// ordinary assistant reply.
//
// It is never shown to the model and never persisted: runTurn.prepareStep
// strips the message fantasy synthesizes from it before the model ever
// sees prepared.Messages (see the Continuation branch there), and run()
// skips createUserMessage entirely for a Continuation call (see the
// call.Continuation guards in run() and persistCanceledTurn). This turn's
// only real content is whatever PrepareStep drains from the completion
// inbox - the exact same drainCompletionsForStep/taskCompletionsMessage
// path the mid-turn fold case already uses, so a completion produces
// identical history and identical model-visible content whether it wakes
// an idle session or folds into a busy one.
const continuationPromptPlaceholder = "(background delegation continuation)"

// startContinuation dispatches an auto-woken continuation turn attempt
// for sessionID. It carries no completions of its own — whatever is
// eligible to wake sits in the dispatcher's completion inbox exactly as
// it does for the mid-turn case, and this call's own PrepareStep (step 0)
// is what drains it, via drainCompletionsForStep, once (and only once)
// this call actually becomes the active turn.
//
// reason is logged, not behavioral: it distinguishes the two callers
// (a completion just arrived and the session was already idle, vs. the
// session just went idle with something already waiting) for anyone
// reading the logs later.
//
// The continuation is dispatched through the exact same a.Run entry
// point every other turn uses — not a bespoke "start streaming" bypass —
// so its own busy-check-and-become-active transition is what actually
// guarantees at most one active turn. If a concurrent call (a real user
// Run, or another continuation attempt racing the same wake) wins that
// race instead, run()'s busy branch drops this one rather than queuing
// it (see the call.Continuation check there): a continuation carries no
// content of its own to fold in, so queuing it would only let its
// placeholder Prompt be persisted later as if it were a real follow-up.
// The completions that triggered this attempt are untouched in that
// case — still safe in the inbox for whichever turn *is* active to pick
// up, either mid-turn or via wakeFromInboxIfIdle when it next goes idle.
//
// A canceled continuation is never requeued: at-most-once is the
// stronger rule. By the time a cancel could reach this turn,
// PrepareStep's own drain already removed the completions from the
// inbox and folded them into prepared.Messages - the model saw them.
// Putting them back would let a retry redeliver content the model
// already received, which is exactly the "task finished twice" failure
// the completion inbox exists to prevent. A step that fails *before*
// that fold succeeds is a different story: drainCompletionsForStep's own
// at-most-once discipline (requeue on a failed step, shared with the
// mid-turn case) still applies, and run()'s exit hook
// (wakeFromInboxIfIdle) will retry it automatically once this failed
// attempt goes idle.
func (a *sessionAgent) startContinuation(ctx context.Context, sessionID, reason string) {
	call := SessionAgentCall{
		SessionID:    sessionID,
		Prompt:       continuationPromptPlaceholder,
		Continuation: true,
	}

	slog.Info("Continuation started", "session", sessionID, "reason", reason)

	// Detached from ctx's cancellation (ctx here is internal/thread's own
	// long-lived lifecycle context, canceled at workspace shutdown) but
	// not from its values, mirroring how Run's own title-generation
	// goroutine (agent.go) detaches from its triggering call's context.
	runCtx := context.WithoutCancel(ctx)
	go func() {
		if _, err := a.Run(runCtx, call); err != nil {
			slog.Error("Auto-continuation turn failed", "session_id", sessionID, "error", err)
		}
	}()
}

// stripContinuationPlaceholder removes the placeholder message
// runTurn.prepareStep expects to find as the last entry of a
// continuation's step-0 messages (see continuationPromptPlaceholder),
// but only after confirming that entry actually *is* the placeholder -
// a single user-role message containing exactly one TextPart equal to
// continuationPromptPlaceholder - rather than trusting fantasy's
// createPrompt to keep putting it last forever. fantasy is a dependency,
// not something this package controls; if its prompt construction ever
// changes, silently stripping whatever happens to be last could delete
// real content, and silently leaving the placeholder in place would leak
// a fabricated user line into every continuation from then on. Neither
// failure mode is acceptable, so a mismatch is reported as an error
// instead - loud and at the exact point the assumption broke, not a
// quiet wrongness discovered later in a transcript.
func stripContinuationPlaceholder(messages []fantasy.Message) ([]fantasy.Message, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("agent: continuation turn has no messages to strip a placeholder from")
	}
	last := messages[len(messages)-1]
	if last.Role != fantasy.MessageRoleUser {
		return nil, fmt.Errorf("agent: continuation turn's last message has role %q, want %q - refusing to strip an unexpected message (fantasy's prompt construction may have changed)", last.Role, fantasy.MessageRoleUser)
	}
	if len(last.Content) != 1 {
		return nil, fmt.Errorf("agent: continuation turn's placeholder message has %d content parts, want 1 (fantasy's prompt construction may have changed)", len(last.Content))
	}
	text, ok := last.Content[0].(fantasy.TextPart)
	if !ok || text.Text != continuationPromptPlaceholder {
		return nil, fmt.Errorf("agent: continuation turn's last message does not match the expected placeholder text (fantasy's prompt construction may have changed)")
	}
	return messages[:len(messages)-1], nil
}

// wakeFromInboxIfIdle re-checks sessionID's completion inbox once this
// session has just gone idle (see run()'s deferred cleanup, the only
// caller) and starts a continuation attempt if anything eligible is
// waiting. This is what catches a completion that arrived while the
// session was busy but whose busy turn never reached another PrepareStep
// to drain it via the ordinary mid-turn path - the common case, since
// most turns are a single step.
func (a *sessionAgent) wakeFromInboxIfIdle(ctx context.Context, sessionID string) {
	if !a.wakeEligible(sessionID) {
		return
	}
	a.startContinuation(ctx, sessionID, "session went idle with a completion already queued")
}
