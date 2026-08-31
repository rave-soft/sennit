package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"charm.land/fantasy"

	"github.com/rave-soft/sennit/internal/session"
)

// maxTaskCascadeDepth bounds how deep delegations may nest: a session a
// person drives is depth 0, a delegation it starts runs at depth 1, a
// delegation started from inside that one at depth 2. A turn already at
// depth 3 still runs — it has real work to do — but may not delegate
// again, so no chain of agents-hiring-agents runs further from the person
// than that.
//
// It bounds nesting only. A session reacting to its own delegation's
// result is the same session at the same level, however many times it
// does so, which is what an iterative plan looks like: implement, review,
// implement again. Counting those rounds as depth (as this once did) shut
// delegation off after three rounds of perfectly ordinary work, while
// leaving real nesting unbounded — a delegation's own turn ran at depth 0
// and could start another at depth 0 forever. How many such rounds a
// session runs is not bounded here or anywhere else.
//
// This is a hard constant, not configuration: the failure mode it guards
// against (agents hiring agents further and further from the person who
// asked) is not something a misconfigured value should be able to reopen.
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

	// A continuation must outlive the short caller context that delivered a
	// completion, but not the coordinator that owns the agent. The coordinator
	// wires its lifecycle context here; standalone agents retain their caller
	// context rather than silently losing cancellation through WithoutCancel.
	runCtx := ctx
	if a.continuationContext != nil {
		runCtx = a.continuationContext()
	}
	// Wait out the backoff for a session whose continuations have been
	// failing. Zero on the ordinary path, so a finished delegation still
	// reaches the model immediately; see continuationRetryBackoff.
	backoff := continuationRetryBackoff(a.continuationFailureCount(sessionID))
	go func() {
		if backoff > 0 {
			timer := time.NewTimer(backoff)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-runCtx.Done():
				return
			}
		}
		var err error
		if a.continuationRunner != nil {
			err = a.continuationRunner(runCtx, sessionID)
		} else {
			_, err = a.Run(runCtx, call)
		}
		failures := a.noteContinuationOutcome(sessionID, err)
		if err == nil {
			return
		}
		if errors.Is(err, session.ErrNotFound) {
			// The session was deleted while its delegation was still
			// running. Nothing will ever drain this inbox, and every
			// attempt to is another failed turn.
			a.dropCompletions(sessionID)
			slog.Warn("Auto-continuation abandoned: session no longer exists", "session_id", sessionID)
			return
		}
		slog.Error("Auto-continuation turn failed", "session_id", sessionID, "error", err, "failures", failures)
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
// session has just gone idle and starts a continuation attempt if anything
// eligible is waiting. This is what catches a completion that arrived
// while the session was busy but whose busy turn never reached another
// PrepareStep to drain it via the ordinary mid-turn path - the common
// case, since most turns are a single step.
//
// Two call sites: runTurn's own deferred cleanup (run_turn.go), the choke
// point every turn exit passes through, and summarize's deferred teardown
// (usage.go) - a summarize holds the active slot for the length of its own
// request, which is exactly as capable of missing a completion as an
// ordinary turn is.
func (a *sessionAgent) wakeFromInboxIfIdle(ctx context.Context, sessionID string) {
	if !a.wakeEligible(sessionID) {
		return
	}
	a.startContinuation(ctx, sessionID, "session went idle with a completion already queued")
}

// SetLiveSession records the session this sennit is working in and then
// re-checks what could not be woken while some other session held that
// standing. The stored id is the whole of the wake path's authority (see
// wakeAllowed), so moving it is itself an event: a completion that was
// refused a wake a minute ago may be eligible the instant it moves.
//
// Without the re-check a report that lands for a session that is not the
// live one has only two ways left to reach it - the person types, or some
// unrelated turn in that session happens to end and run
// wakeFromInboxIfIdle. A delegation that *failed* is exactly the case
// where neither happens: the session has nothing else in flight to end,
// and the person is sitting in front of a session that has visibly
// stopped. Observed in the wild as a five-minute stall, ended only by an
// idle-summarize teardown that happened to take the wake path on its way
// out.
//
// Overrides the promoted dispatcher method of the same name; every caller
// reaches it through the SessionAgent interface, so the sweep is not
// bypassable by reaching for the embedded field.
func (a *sessionAgent) SetLiveSession(sessionID string) {
	a.dispatcher.SetLiveSession(sessionID)
	if sessionID == "" {
		// The landing screen: nothing is live, so nothing is wakeable.
		return
	}
	a.wakeQueuedForLiveSession()
}

// wakeQueuedForLiveSession starts a continuation attempt for every
// session that now has both something queued and the standing to be
// woken for it.
//
// It sweeps every session rather than the newly live one alone because
// wakeAllowed's authority covers a whole tree: a delegation of the live
// session, parked waiting on children of its own, is exactly as
// unwakeable while another tree is live, and exactly as stuck once it is
// not. wakeEligible re-applies the full gate (live tree, idle, not
// cancelled, still has an inbox) per session, so a sweep can only start
// turns that were already owed one.
func (a *sessionAgent) wakeQueuedForLiveSession() {
	for sessionID := range a.states.Seq2() {
		if !a.wakeEligible(sessionID) {
			continue
		}
		a.startContinuation(context.Background(), sessionID,
			"session became the one being worked in with a completion already queued")
	}
}
