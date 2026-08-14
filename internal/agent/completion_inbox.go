package agent

import (
	"fmt"
	"strings"

	"charm.land/fantasy"
)

// TaskCompletion is the structured event delivered into a session's
// completion inbox when a background delegation (currently: a task; see
// internal/thread's TaskManager) reaches a terminal status. It carries
// everything runTurn.prepareStep needs to describe the outcome to the
// model without the model having to poll task_result first.
//
// This is deliberately not a notify.RunComplete: RunComplete is a
// broadcast, fire-and-once pubsub event with no persistence contract,
// while a completion must be delivered to exactly one place (the
// parent session's next step) exactly once, surviving a failed step —
// see dispatcher's completion-inbox methods below.
type TaskCompletion struct {
	// DelegationID is the task's own id (internal/thread's Delegation.ID).
	DelegationID string
	// Kind is the delegation kind ("task" today; see internal/thread's
	// Kind). Carried through rather than assumed, so the model's-eye
	// view stays accurate if a second kind is ever wired into this same
	// inbox.
	Kind string
	// Name and Goal identify what ran: the delegation's (generated) name
	// and the prompt it was given.
	Name string
	Goal string
	// Status is the terminal status the delegation rested at (e.g.
	// "completed", "failed", "interrupted").
	Status string
	// ChildSessionID is the task's own session - never the delivery
	// target. Delivery always targets the *parent* session (resolved via
	// session.Session.ParentSessionID by the caller of
	// Coordinator.DeliverTaskCompletion), so nothing here decides
	// placement; it is included purely for the model's benefit (e.g. to
	// address a follow-up task_send at the right id).
	ChildSessionID string
	// ResultText is the final answer on success; empty otherwise.
	ResultText string
	// Error is the failure text on failure; empty otherwise.
	Error string
}

// enqueueCompletion appends completion to sessionID's completion inbox.
// It never interrupts a running turn and never starts one on an idle
// session - both cases just leave the event queued for the session's
// next prepareStep (or, for an idle session, a future step - see the
// dispatcher doc comment on completionInbox for what's not built yet).
//
// Locking mirrors enqueueCall/requeueContinuation: the per-session
// dispatch mutex also guards this session's completion inbox, so a
// concurrent drain (prepareStep) and enqueue (a task finishing
// mid-step) can never interleave into a torn read.
func (d *dispatcher) enqueueCompletion(sessionID string, completion TaskCompletion) {
	mu := d.sessionMu(sessionID)
	mu.Lock()
	defer mu.Unlock()
	existing, _ := d.completionInbox.Get(sessionID)
	d.completionInbox.Set(sessionID, append(existing, completion))
}

// drainCompletionsForStep removes and returns every completion queued for
// sessionID, for prepareStep to fold into the current step ahead of
// steering follow-ups. Unlike drainQueueForStep there is no cancel-mark
// filtering to apply: a completion was never "accepted" the way a
// prompt is, so nothing about a session cancel makes one stale.
func (d *dispatcher) drainCompletionsForStep(sessionID string) []TaskCompletion {
	mu := d.sessionMu(sessionID)
	mu.Lock()
	defer mu.Unlock()
	drained, ok := d.completionInbox.Get(sessionID)
	if !ok || len(drained) == 0 {
		return nil
	}
	d.completionInbox.Del(sessionID)
	return drained
}

// requeueCompletions puts remainder - a suffix of a batch drained by
// drainCompletionsForStep that the caller failed to fold into the step -
// back at the front of sessionID's completion inbox. It exists for the
// same reason requeueDrained exists for steering messages: if folding
// fails partway through, the completions not yet delivered must not be
// lost, and a duplicated completion (delivered once, then replayed on
// retry) would be worse than a lost one - it would tell the model a
// task finished twice.
//
// remainder is prepended, not merged by sequence: completions carry no
// accept-sequence the way queued calls do, and remainder was drained
// strictly before anything currently in the inbox could have been
// enqueued (drain happens at the start of the step that failed), so a
// plain prepend already preserves arrival order.
func (d *dispatcher) requeueCompletions(sessionID string, remainder []TaskCompletion) {
	if len(remainder) == 0 {
		return
	}
	mu := d.sessionMu(sessionID)
	mu.Lock()
	defer mu.Unlock()
	existing, _ := d.completionInbox.Get(sessionID)
	merged := make([]TaskCompletion, 0, len(remainder)+len(existing))
	merged = append(merged, remainder...)
	merged = append(merged, existing...)
	d.completionInbox.Set(sessionID, merged)
}

// taskCompletionsMessage renders completions as a single user-role
// message, plainly labeled as a system-generated report rather than
// something the user typed.
//
// This was originally a system-role message (fantasy.NewSystemMessage),
// which is wrong: a fantasy.Prompt can carry at most one *effective*
// system block on Anthropic and Google - both adapters group consecutive
// system messages into a block, then silently drop (continue, no
// warning) any later block once a user/assistant message has
// interrupted it (providers/anthropic/anthropic.go and
// providers/google/google.go, both `if finishedSystemBlock { continue }`
// in their prompt-conversion loop). A completion folded in after the
// turn's original system prompt and any prior steps' messages - which it
// always is - hit exactly that case and vanished before reaching the
// model on those two providers, while silently working on OpenAI (chat
// and Responses) and openaicompat, which convert every system message
// independently regardless of position. A mechanism that only some
// providers implement correctly is worse than a uniformly plain one.
//
// A user-role message has no such trap: PROVIDER_ORDERING_SPIKE.md
// already established, for steering, that fantasy accepts (and on
// Anthropic, merges) a user message appended after other turns - the
// same path this rides now. The plan's constraint was that a completion
// must not be *queued as a user prompt* or attributed to the user in
// history (see dispatcher's completionInbox doc comment: this never goes
// through createUserMessage/message.Service, so nothing here is
// persisted or shows up as something the user said) - not that
// fantasy.MessageRoleUser can never carry it on the wire. The leading
// label in formatTaskCompletion's text is what keeps the model itself
// from misreading it as user speech.
func taskCompletionsMessage(completions []TaskCompletion) fantasy.Message {
	parts := make([]string, len(completions))
	for i, c := range completions {
		parts[i] = formatTaskCompletion(c)
	}
	return fantasy.NewUserMessage(strings.Join(parts, "\n\n"))
}

// formatTaskCompletion renders one completion as plain text for the
// model: what ran, how it ended, and its result or error. The leading
// line exists specifically so the model does not mistake this for
// something the user said - see taskCompletionsMessage.
func formatTaskCompletion(c TaskCompletion) string {
	var b strings.Builder
	b.WriteString("[system-generated delegation report - not user input]\n")
	fmt.Fprintf(&b, "A background %s has finished.\n", c.Kind)
	fmt.Fprintf(&b, "id: %s\n", c.DelegationID)
	fmt.Fprintf(&b, "name: %s\n", c.Name)
	fmt.Fprintf(&b, "goal: %s\n", c.Goal)
	fmt.Fprintf(&b, "status: %s\n", c.Status)
	fmt.Fprintf(&b, "child_session: %s\n", c.ChildSessionID)
	if c.Error != "" {
		fmt.Fprintf(&b, "\nError:\n%s", c.Error)
	} else {
		fmt.Fprintf(&b, "\nResult:\n%s", c.ResultText)
	}
	return b.String()
}
