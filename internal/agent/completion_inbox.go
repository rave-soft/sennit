package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

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
	// Depth is the background-delegation cascade depth of the turn that
	// created the delegation this completion reports on (0 for a real
	// user turn). An auto-woken continuation started from this
	// completion runs at Depth+1 — see startContinuation and the "agent"
	// tool's background mode, which refuses to start further background
	// work once that reaches the hard cascade limit.
	Depth int
	// TerminalAt is when this event was built for delivery — stamped
	// once, a few synchronous instructions after whatever triggered it
	// (the terminal status write, for a completion; the SendToParent
	// call, for a message), and read back by runTurn.prepareStep to
	// report how long the event sat before reaching the model, without a
	// second clock reading anywhere else that could drift from this one.
	// The name predates IsMessage below; it is kept rather than renamed
	// to BuiltAt so every existing terminal-completion call site (all of
	// internal/thread's lifecycle.deliverCompletion) needs no change.
	TerminalAt time.Time
	// Acknowledge clears durable outbox state after successful model delivery.
	Acknowledge func(context.Context) error
	// IsMessage distinguishes a mid-run ask (SendToParent) from a
	// terminal completion. False (the zero value) for every existing
	// call site, so this inbox's established behavior for a completion
	// is unchanged. When true, Status/ResultText/Error are unused (left
	// zero) and Message carries the ask text instead.
	IsMessage bool
	// Message is the ask text for a mid-run event (IsMessage true).
	// Unused for a terminal completion.
	Message string
}

// enqueueCompletion appends completion to sessionID's completion inbox
// and reports whether the session is now eligible for an auto-wake
// attempt (idle, not left canceled by the user). It never interrupts a
// running turn itself - eligible only ever means "the caller may attempt
// a continuation," never "this call drained anything." The actual
// consumption happens later, atomically, in whichever turn's own
// PrepareStep (step 0, for a continuation; any step, for the mid-turn
// fold) ends up draining the inbox via drainCompletionsForStep - see
// that method and startContinuation.
//
// Locking mirrors enqueueCall/requeueContinuation: the session's own
// dispatch mutex also guards its completion inbox, so a concurrent drain
// (PrepareStep) and enqueue (a task finishing mid-step) can never
// interleave into a torn read, and the eligibility read below can never
// observe a stale busy/canceled state a concurrent Run has already
// changed by the time this returns.
func (d *dispatcher) enqueueCompletion(sessionID string, completion TaskCompletion) (wakeEligible bool) {
	s, release := d.session(sessionID)
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completionInbox = append(s.completionInbox, completion)
	// A new completion is a new event, not another go at the one that
	// kept failing: give the wake path its attempts back. The cap still
	// bounds the loop, since each reset costs an external event.
	s.continuationFailures = 0
	// ids, kind, status, and the is-message flag only — never
	// completion.Goal, .ResultText, .Error, or .Message, which are the
	// user's own work (or the delegation's own words) and must not end
	// up in a log that outlives the session.
	slog.Info("Completion enqueued", "delegation", completion.DelegationID, "kind", completion.Kind, "status", completion.Status, "is_message", completion.IsMessage, "session", sessionID, "inbox_size", len(s.completionInbox))
	return d.wakeEligibleLocked(s)
}

// drainCompletionsForStep removes and returns every completion queued for
// sessionID, for prepareStep to fold into the current step ahead of
// steering follow-ups. Unlike drainQueueForStep there is no cancel-mark
// filtering to apply: a completion was never "accepted" the way a
// prompt is, so nothing about a session cancel makes one stale.
func (d *dispatcher) drainCompletionsForStep(sessionID string) []TaskCompletion {
	s, release := d.session(sessionID)
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.completionInbox) == 0 {
		return nil
	}
	drained := s.completionInbox
	s.completionInbox = nil
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
	s, release := d.session(sessionID)
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	merged := make([]TaskCompletion, 0, len(remainder)+len(s.completionInbox))
	merged = append(merged, remainder...)
	merged = append(merged, s.completionInbox...)
	s.completionInbox = merged
}

// maxContinuationAttempts bounds how many auto-woken continuation turns
// may fail in a row for one completion batch. Three is enough to ride out
// a transient provider failure and small enough that a permanent one
// (a deleted session) costs a handful of attempts rather than a loop.
const maxContinuationAttempts = 3

// continuationRetryBackoff is the pause before a retry after n consecutive
// failures. Zero for the first attempt: the ordinary wake must stay
// immediate, since it is what makes a finished delegation feel like it
// reached the agent at once.
func continuationRetryBackoff(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	return time.Duration(failures) * time.Second
}

// noteContinuationOutcome records how an auto-woken continuation ended and
// reports the consecutive failure count. A success clears the count.
func (d *dispatcher) noteContinuationOutcome(sessionID string, err error) int {
	s, release := d.session(sessionID)
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.continuationFailures = 0
		return 0
	}
	s.continuationFailures++
	return s.continuationFailures
}

// continuationFailureCount reports the consecutive failure count, for the
// backoff a retry waits out before it starts.
func (d *dispatcher) continuationFailureCount(sessionID string) int {
	s, release := d.session(sessionID)
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.continuationFailures
}

// dropCompletions discards everything queued for sessionID. It is for the
// one case where the queue can never be delivered: the session itself is
// gone, so no turn will ever run to drain it and holding the events only
// keeps the dispatch state alive.
func (d *dispatcher) dropCompletions(sessionID string) {
	s, release := d.session(sessionID)
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.completionInbox) == 0 {
		return
	}
	slog.Warn("Dropping queued completions for a session that no longer exists",
		"session", sessionID, "inbox_size", len(s.completionInbox))
	s.completionInbox = nil
}

// wakeEligible reports whether sessionID currently has something in its
// completion inbox and is eligible for an auto-wake attempt (idle, not
// left canceled by the user). It exists for run()'s exit path: a
// completion can arrive while a session is busy and never get the chance
// to be drained via that turn's own PrepareStep (a turn with no further
// step after the one it was busy on - the common case), so something has
// to look again once the turn that was busy actually goes idle. See
// wakeFromInboxIfIdle, the only caller.
//
// This is a peek, not a drain: nothing is removed from the inbox here.
// The actual consumption happens later, atomically, in whichever turn's
// own PrepareStep drains it via drainCompletionsForStep - see
// startContinuation for why a separate pre-drain step would reintroduce
// exactly the "fabricated user message" problem this design avoids.
func (d *dispatcher) wakeEligible(sessionID string) bool {
	s, release := d.session(sessionID)
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	return d.wakeEligibleLocked(s)
}

// wakeEligibleLocked is wakeEligible's body. Callers must hold s.mu -
// see enqueueCompletion's doc comment for why this decision has to be
// made under that same lock a normal run's own busy-check uses.
func (d *dispatcher) wakeEligibleLocked(s *sessionState) bool {
	if len(s.completionInbox) == 0 {
		return false
	}
	if s.active != nil {
		return false
	}
	if s.continuationFailures >= maxContinuationAttempts {
		// Something about this session keeps the continuation from
		// running at all — its row is gone, its history will not load.
		// Stop attempting: the completions stay queued and reach the
		// model the ordinary way on whatever the next real turn is.
		slog.Warn("Delivery skipped: continuation keeps failing, completion left queued",
			"failures", s.continuationFailures, "inbox_size", len(s.completionInbox))
		return false
	}
	if s.cancelled {
		// The user explicitly canceled this session and no new turn has
		// started since. Leave the event queued; it is delivered the
		// ordinary way (drainCompletionsForStep) on whatever the next
		// real user turn turns out to be.
		slog.Info("Delivery skipped: session cancelled, completion left queued", "inbox_size", len(s.completionInbox))
		return false
	}
	return true
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
// A user-role message has no such trap: the provider-ordering spike
// established, for steering, that fantasy accepts (and on
// Anthropic, merges) a user message appended after other turns - the
// same path this rides now. The plan's constraint was that a completion
// must not be *queued as a user prompt* or attributed to the user in
// history - not that fantasy.MessageRoleUser can never carry it on the
// wire. This never goes through createUserMessage/message store (it is
// folded directly into prepared.Messages, not persisted) whether it is
// reached from the mid-turn fold or from a continuation's own step 0
// (runTurn.prepareStep's Continuation branch calls this exact function
// too - the two delivery paths share this one code path deliberately, so
// they cannot drift into recording the same event differently); the
// leading label in joinTaskCompletions' text is what keeps the model
// itself from misreading it as user speech.
func taskCompletionsMessage(completions []TaskCompletion) fantasy.Message {
	return fantasy.NewUserMessage(joinTaskCompletions(completions))
}

// joinTaskCompletions renders a batch of completions as one block of
// plain text, wrapped as a fantasy message by taskCompletionsMessage -
// the sole caller, used identically whether that message ends up folded
// into an active turn or into a continuation's own step 0.
func joinTaskCompletions(completions []TaskCompletion) string {
	parts := make([]string, len(completions))
	for i, c := range completions {
		parts[i] = formatTaskCompletion(c)
	}
	return strings.Join(parts, "\n\n")
}

// formatTaskCompletion renders one inbox event as plain text for the
// model, branching on IsMessage so a mid-run ask can never be mistaken
// for "a background X finished" and vice versa. The leading line exists
// specifically so the model does not mistake either shape for something
// the user said - see taskCompletionsMessage. Both shapes include the
// delegation's id, kind, and name so the parent can address a reply
// (thread_send/task_send) at the right target.
func formatTaskCompletion(c TaskCompletion) string {
	if c.IsMessage {
		return formatDelegationMessage(c)
	}
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

// formatDelegationMessage renders a mid-run ask (IsMessage true) as
// plain text for the model. Its leading line deliberately reads
// differently from formatTaskCompletion's "has finished" line - a
// running delegation asking something is not the same event as one
// reaching a terminal status, and the model must be able to tell them
// apart at a glance to decide whether a reply is even expected.
func formatDelegationMessage(c TaskCompletion) string {
	var b strings.Builder
	b.WriteString("[system-generated delegation report - not user input]\n")
	fmt.Fprintf(&b, "A running background %s is asking you something.\n", c.Kind)
	fmt.Fprintf(&b, "id: %s\n", c.DelegationID)
	fmt.Fprintf(&b, "name: %s\n", c.Name)
	fmt.Fprintf(&b, "child_session: %s\n", c.ChildSessionID)
	fmt.Fprintf(&b, "\nMessage:\n%s", c.Message)
	return b.String()
}
