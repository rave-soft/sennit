package agent

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rave-soft/sennit/internal/csync"
)

// dispatcher owns the "accept/queue/cancel" dispatch protocol shared by
// sessionAgent.Run and sessionAgent.Summarize: tracking which sessions are
// active, which prompts have been dispatched but not yet started, and which
// sessions carry a pending cancel that must poison prompts accepted before
// it. It intentionally has no dependency on pubsub or message persistence -
// callers (sessionAgent) are responsible for publishing RunComplete events
// for whatever this type reports as dropped.
type dispatcher struct {
	messageQueue   *csync.Map[string, []SessionAgentCall]
	activeRequests *csync.Map[string, *activeCancel]

	// dispatchMu holds a per-session mutex that serializes the
	// accepted -> (cancel-on-entry | queued | active) transition in
	// Run against a concurrent Cancel. The lock is held only during
	// the brief handoff (no DB or LLM I/O under the lock).
	//
	// Entries are never removed: sessionMu below hands out the *sync.Mutex
	// pointer without holding dispatchMuCreate on the fast path, so a
	// caller can be holding (or about to look up) a session's mutex at
	// the same time a cleanup elsewhere decides the session is idle and
	// deletes it. If a new mutex were then created for the same session,
	// the two instances would no longer exclude each other, silently
	// breaking the invariant this map exists to provide. Safe removal
	// needs a refcount on top of the mutex (increment under
	// dispatchMuCreate in sessionMu, decrement - and delete at zero - when
	// the caller is done with it), which is a bigger change than this
	// leak justifies: the map is bounded by the number of distinct
	// sessions touched over the process lifetime, not by request volume.
	dispatchMu *csync.Map[string, *sync.Mutex]
	// completionInbox holds per-session TaskCompletion events - internal
	// notifications that a background task finished, kept separate from
	// messageQueue because a completion is not a steering follow-up and
	// must never be folded in as if the user had typed it. It lives here
	// rather than in a new type specifically so it inherits dispatcher's
	// existing shape (in-memory, per-session, no persistence, no
	// pubsub): a completion is durably recorded in internal/thread's
	// store first (that row is what task_result polls), and this inbox
	// only carries the lossy, at-most-once-delivery copy of it that
	// prepareStep drains - if the process dies with an event still
	// queued here, the underlying task is still terminal and can still
	// be polled, so nothing but the (already best-effort) push
	// notification is lost.
	//
	// See enqueueCompletion/drainCompletionsForStep/requeueCompletions
	// in completion_inbox.go for the operations, and runTurn.prepareStep
	// for the drain-before-steering ordering, and the Continuation
	// branch that drains this same way for a continuation's own step 0.
	// wakeEligible (also completion_inbox.go) is the idle-session
	// trigger: when a session is idle, not user-canceled, and this map
	// holds something for it, sessionAgent attempts a continuation turn
	// instead of leaving the event to wait indefinitely - see
	// startContinuation. It only decides to attempt, never drains: the
	// actual consumption always happens in PrepareStep, so the mid-turn
	// and wake paths can never record the same event differently.
	completionInbox *csync.Map[string, []TaskCompletion]
	// delegationParents maps a running delegation's own (child) session
	// id to where its mid-run asks (SendToParent) should be delivered.
	// Registered once, at delegation-create time, by internal/thread -
	// separate from completionInbox because a parent target must be
	// resolvable from *inside* the delegation's own turn, on demand,
	// unlike a terminal completion's target, which is resolved once,
	// externally, and handed straight to DeliverTaskCompletion. See
	// DelegationParent and RegisterDelegationParent.
	delegationParents *csync.Map[string, DelegationParent]
	// cancelledSessions marks a session as "the user explicitly canceled
	// this" until the next turn actually starts (see run's idle branch,
	// which clears it). It exists solely to gate auto-waking a
	// continuation from the completion inbox: cancelMark above is the
	// wrong signal for that (it is scoped to covering accepted-but-not-
	// active runs and is dropped once none remain — see endAccepted —
	// so by the time a session is genuinely idle with an empty queue, a
	// plain Escape has usually already cleared it). Presence means
	// canceled; absence means not. Set only by cancel(); cleared only by
	// run() admitting a new active turn, whichever call — a real user
	// Run/Steer or our own auto-continuation — gets there next.
	cancelledSessions *csync.Map[string, struct{}]
	// acceptedRuns counts dispatched-but-not-yet-active runs per
	// session. A counter > 0 means a dispatched prompt is in flight
	// and has not yet completed the dispatch handoff in Run. Only
	// BeginAccepted increments it; only AcceptedRun.Close decrements
	// it.
	acceptedRuns *csync.Map[string, int]
	// cancelMark records, per session, a high-water accept sequence: an
	// accepted handle is canceled by it iff the handle's sequence is at
	// or below the mark. Cancel raises the mark to the latest sequence
	// assigned at cancel time, so a single Cancel covers every prompt
	// accepted-but-not-yet-active then, while a prompt accepted later
	// (higher sequence) is never poisoned. Absent or 0 means no pending
	// cancel. It is only raised by Cancel when acceptedRuns > 0, so an
	// idle Escape never records a mark.
	cancelMark *csync.Map[string, uint64]
	// dispatchMuCreate guards lazy creation of per-session entries in
	// dispatchMu so two goroutines can't race to lock different mutex
	// instances for the same session.
	dispatchMuCreate sync.Mutex
	// acceptedMu serializes increments/decrements of acceptedRuns and
	// the assignment of accept sequence numbers from acceptSeqGen. It
	// is separate from dispatchMu so AcceptedRun.Close (which may run
	// while Run holds dispatchMu for the same session) does not
	// deadlock by re-entering the dispatch lock.
	acceptedMu sync.Mutex
	// acceptSeqGen is the monotonic source of accept sequence numbers.
	// Each BeginAccepted increments it under acceptedMu and stamps the
	// returned handle, so sequences strictly increase in accept order
	// across the agent. Cancel uses its current value as the per-session
	// high-water mark.
	acceptSeqGen uint64
	// onQueueChanged, when non-nil, is called (with the per-session
	// dispatch mutex already released - see notifyQueueChanged) whenever
	// a mutation actually changes what's queued for a session: enqueue,
	// drain, requeue, cancel, or clear. It exists so an owner
	// (sessionAgent) can publish a lossy "the queue changed" signal - the
	// UI's queued-prompt pill refreshes off it - without this type
	// depending on pubsub itself, preserving the doc comment above. Set
	// once at construction; not guarded by a lock since it never changes
	// after that.
	onQueueChanged func(sessionID string)
}

// DelegationParent describes where a running delegation should send an
// ask, and how to attribute it. Registered once, at delegation-create
// time, by internal/thread (a later change - not part of this step),
// keyed by the delegation's own (child) session id.
type DelegationParent struct {
	// Parent is the Coordinator owning the parent session's completion
	// inbox. For a task this is the delegation's own Coordinator (a
	// task shares its parent's App/coordinator); for a thread with a
	// parent it is a different Coordinator entirely (the thread spawns
	// its own isolated App) - see internal/thread's
	// resolveDeliveryTarget for the existing analogous split on the
	// terminal-completion path.
	Parent          Coordinator
	ParentSessionID string
	DelegationID    string
	Kind            string
	Name            string
	Depth           int
}

func newDispatcher() *dispatcher {
	return &dispatcher{
		messageQueue:      csync.NewMap[string, []SessionAgentCall](),
		activeRequests:    csync.NewMap[string, *activeCancel](),
		dispatchMu:        csync.NewMap[string, *sync.Mutex](),
		acceptedRuns:      csync.NewMap[string, int](),
		cancelMark:        csync.NewMap[string, uint64](),
		completionInbox:   csync.NewMap[string, []TaskCompletion](),
		cancelledSessions: csync.NewMap[string, struct{}](),
		delegationParents: csync.NewMap[string, DelegationParent](),
	}
}

// RegisterDelegationParent records where sessionID (a delegation's own
// child session) should deliver a mid-run ask - see DelegationParent and
// SendToParent. A plain Set: a later registration for the same session
// id simply replaces the earlier one, which is fine since a session only
// ever has one parent for its lifetime.
func (d *dispatcher) RegisterDelegationParent(sessionID string, parent DelegationParent) {
	d.delegationParents.Set(sessionID, parent)
}

// AcceptedRun owns exactly one accept reservation taken by
// BeginAccepted. It is the only carrier of accept-state across the
// backend.runAgent / Coordinator.Run / sessionAgent.Run layers: a
// counter > 0 means a dispatched prompt is in flight and has not yet
// completed the dispatch handoff in Run. Close is the only way to
// release the reservation and is idempotent.
type AcceptedRun struct {
	d         *dispatcher
	sessionID string
	// seq is the monotonic accept sequence stamped by BeginAccepted. A
	// cancel covers this handle iff seq is at or below the session's
	// cancel mark, so a handle accepted after a cancel (higher seq) is
	// never poisoned by it.
	seq  uint64
	done atomic.Bool
}

// Close decrements the accept counter for this reservation. It is safe
// to call multiple times; only the first call has effect.
func (r *AcceptedRun) Close() {
	if r == nil {
		return
	}
	if !r.done.CompareAndSwap(false, true) {
		return
	}
	r.d.endAccepted(r.sessionID)
}

// SessionID exposes the session this reservation is for so the run path
// can use it without an extra parameter.
func (r *AcceptedRun) SessionID() string {
	if r == nil {
		return ""
	}
	return r.sessionID
}

// BeginAccepted increments the accept counter for sessionID and returns
// a handle whose Close is the only way to decrement it. It is the only
// entry point that mutates acceptedRuns.
func (d *dispatcher) BeginAccepted(sessionID string) *AcceptedRun {
	d.acceptedMu.Lock()
	defer d.acceptedMu.Unlock()
	count, _ := d.acceptedRuns.Get(sessionID)
	d.acceptedRuns.Set(sessionID, count+1)
	d.acceptSeqGen++
	return &AcceptedRun{d: d, sessionID: sessionID, seq: d.acceptSeqGen}
}

// endAccepted decrements the accept counter for sessionID. It is only
// called via AcceptedRun.Close. It uses a dedicated lock (not the
// per-session dispatch mutex) so it can run while Run holds dispatchMu
// for the same session without deadlocking.
//
// When the count reaches zero the session's cancel mark is dropped: no
// accepted handle remains for it to cover, and any handle accepted later
// gets a strictly higher sequence that the mark would not match anyway.
// Handles canceled on entry never reach RunComplete, so this is the only
// place that clears the mark for an all-canceled batch. Sibling handles
// covered by the same mark are serialized on the per-session dispatch
// mutex and read the mark before they Close, so this never clears it out
// from under a covered handle still waiting to enter Run.
func (d *dispatcher) endAccepted(sessionID string) {
	d.acceptedMu.Lock()
	defer d.acceptedMu.Unlock()
	count, ok := d.acceptedRuns.Get(sessionID)
	if !ok || count <= 1 {
		d.acceptedRuns.Del(sessionID)
		d.cancelMark.Del(sessionID)
		return
	}
	d.acceptedRuns.Set(sessionID, count-1)
}

// sessionMu returns the per-session dispatch mutex, creating it on first
// use. Creation is guarded so concurrent callers always observe the same
// mutex instance for a given session.
func (d *dispatcher) sessionMu(sessionID string) *sync.Mutex {
	if mu, ok := d.dispatchMu.Get(sessionID); ok {
		return mu
	}
	d.dispatchMuCreate.Lock()
	defer d.dispatchMuCreate.Unlock()
	if mu, ok := d.dispatchMu.Get(sessionID); ok {
		return mu
	}
	mu := &sync.Mutex{}
	d.dispatchMu.Set(sessionID, mu)
	return mu
}

// notifyQueueChanged invokes onQueueChanged if one is set. Every caller
// - internal (the self-locking methods below, via a defer registered
// before the mutex's own so it runs after the mutex's Unlock) and
// external (run's busy-enqueue branch, after its own explicit Unlock) -
// must call this only once the per-session dispatch mutex has been
// released: onQueueChanged ultimately reaches a pubsub broker, whose
// subscriber callbacks are unbounded work that must never run under a
// lock documented as covering a brief, I/O-free handoff.
func (d *dispatcher) notifyQueueChanged(sessionID string) {
	if d.onQueueChanged != nil {
		d.onQueueChanged(sessionID)
	}
}

// enqueueCall appends call to the session's message queue. The
// OnComplete hook is stripped: the caller that supplied it (typically
// coordinator.Run) has its own retry/coalesce scope that ends when it
// returns, so by the time the queue drains nobody is left to consume the
// buffered terminal event. The recursive Run falls back to the default
// broker publish, which is what existing subscribers expect for queued
// turns.
//
// Unlike the methods below, enqueueCall does not call notifyQueueChanged
// itself: its only caller (run's busy branch) already holds the
// per-session dispatch mutex when it calls this, so notifying here would
// violate the "never under the lock" rule. run calls notifyQueueChanged
// explicitly, right after its own Unlock.
func (d *dispatcher) enqueueCall(call SessionAgentCall) {
	existing, ok := d.messageQueue.Get(call.SessionID)
	if !ok {
		existing = []SessionAgentCall{}
	}
	queued := call
	if call.Accepted != nil {
		// Preserve the accept sequence after the handle is stripped so
		// the queue-drain paths can tell a follow-up queued before a
		// cancel (covered by the mark) from one queued after it.
		queued.acceptSeq = call.Accepted.seq
	}
	queued.OnComplete = nil
	queued.Accepted = nil
	// The single stamp this measurement rests on — see queuedAt's own
	// doc comment.
	queued.queuedAt = time.Now()
	existing = append(existing, queued)
	d.messageQueue.Set(call.SessionID, existing)
}

func (d *dispatcher) requeueContinuation(call SessionAgentCall, onQueued func()) {
	mu := d.sessionMu(call.SessionID)
	// Registered before the Unlock defer below so it runs after it (defers
	// run LIFO): this always appends, so the queue always changes.
	defer d.notifyQueueChanged(call.SessionID)
	mu.Lock()
	defer mu.Unlock()

	existing, ok := d.messageQueue.Get(call.SessionID)
	if !ok {
		existing = []SessionAgentCall{}
	}
	d.messageQueue.Set(call.SessionID, append(existing, call))
	onQueued()
}

// drainQueueForStep partitions the session's queued calls for the current
// streaming step under the per-session dispatch mutex so the filtering is
// atomic against a concurrent Cancel: canceledBySeq requires the caller to
// hold that mutex, and evaluating it here (rather than after unlocking)
// prevents a cancel recorded between the drain and the check from being
// observed inconsistently.
//
// Calls covered by a pending cancel are dropped; the dropped ones that
// carry a RunID are returned in canceledWithRunID so the caller can
// publish their terminal cancelled RunComplete (a caller waiting on that
// RunID, e.g. `braid run`, would otherwise hang). Uncanceled calls without
// a RunID are returned in fold to be folded into the active turn,
// preserving the existing follow-up behavior. Uncanceled calls that carry
// a RunID are left in the queue so each runs as its own turn via the
// recursive run path and publishes its own RunComplete, giving every
// RunID-bearing prompt an explicit lifecycle instead of being silently
// absorbed into another turn. fold is processed by the caller without the
// lock held.
func (d *dispatcher) drainQueueForStep(sessionID string) (fold, canceledWithRunID []SessionAgentCall) {
	dispatchLock := d.sessionMu(sessionID)
	// Registered before the Unlock defer below so it runs after it (defers
	// run LIFO), reading the named returns once they're final. Only
	// notify when something actually left the queue - drainQueueForStep
	// runs on every step of every turn, most of which find nothing
	// queued.
	defer func() {
		if len(fold) > 0 || len(canceledWithRunID) > 0 {
			d.notifyQueueChanged(sessionID)
		}
	}()
	dispatchLock.Lock()
	defer dispatchLock.Unlock()
	queuedCalls, _ := d.messageQueue.Get(sessionID)
	var keep []SessionAgentCall
	for _, queued := range queuedCalls {
		if d.canceledBySeq(sessionID, queued.acceptSeq) {
			if queued.RunID != "" {
				canceledWithRunID = append(canceledWithRunID, queued)
			}
			continue
		}
		if queued.RunID != "" {
			keep = append(keep, queued)
			continue
		}
		fold = append(fold, queued)
	}
	if len(keep) == 0 {
		d.messageQueue.Del(sessionID)
	} else {
		d.messageQueue.Set(sessionID, keep)
	}
	return fold, canceledWithRunID
}

// drainNext filters sessionID's queued calls against the cancel mark and,
// if any survive, reserves a fresh accept for the first and pops it off the
// queue. It is the single implementation of "hand off to the next queued
// prompt", shared by Run's end-of-turn handoff and Summarize's
// post-completion handoff — closing the gap where the Summarize path used
// to skip the cancel-mark check entirely (a prompt queued during
// summarization and canceled would run anyway).
//
// queued is the filtered queue as it stood before popping (callers need
// RunID membership to decide whether they still owe their own
// RunComplete). next is the popped call with a fresh accept reservation,
// or nil if nothing survived. canceledWithRunID are dropped calls that
// carried a RunID and need a terminal cancelled RunComplete published by
// the caller.
func (d *dispatcher) drainNext(sessionID string) (queued []SessionAgentCall, next *SessionAgentCall, canceledWithRunID []SessionAgentCall) {
	mu := d.sessionMu(sessionID)
	// changed tracks whether the queue held anything to begin with:
	// every branch below either drops entries (cancel-mark filtering) or
	// pops one (the handoff), so a non-empty starting queue always ends
	// up different. Registered before the Unlock defer so it runs after
	// it (defers run LIFO).
	var changed bool
	defer func() {
		if changed {
			d.notifyQueueChanged(sessionID)
		}
	}()
	mu.Lock()
	defer mu.Unlock()

	queuedMessages, _ := d.messageQueue.Get(sessionID)
	changed = len(queuedMessages) > 0
	if mark, ok := d.cancelMark.Get(sessionID); ok && mark > 0 && len(queuedMessages) > 0 {
		// A cancel was recorded for this session (e.g. it arrived while
		// this run was active and follow-ups had been queued). Drop the
		// queued prompts it covers (accept sequence at or below the
		// mark, or untracked); keep any queued after the cancel (higher
		// sequence) so they still run.
		var kept []SessionAgentCall
		for _, q := range queuedMessages {
			if q.acceptSeq == 0 || q.acceptSeq <= mark {
				if q.RunID != "" {
					canceledWithRunID = append(canceledWithRunID, q)
				}
				continue
			}
			kept = append(kept, q)
		}
		queuedMessages = kept
		d.messageQueue.Set(sessionID, kept)
	}

	if len(queuedMessages) == 0 {
		// No queued work. Clear the cancel mark only when no accepted
		// run remains in flight that it might still cover; otherwise a
		// sibling prompt (sequence at or below the mark) waiting to
		// enter Run would lose its cancellation. When accepted runs are
		// gone, this also clears a stale mark so it can't catch a
		// future run.
		d.messageQueue.Del(sessionID)
		d.acceptedMu.Lock()
		inFlight, _ := d.acceptedRuns.Get(sessionID)
		d.acceptedMu.Unlock()
		if inFlight == 0 {
			d.cancelMark.Del(sessionID)
		}
		return nil, nil, canceledWithRunID
	}

	// Reserve a fresh accept for the dequeued prompt before dropping the
	// lock so acceptedRuns > 0 across the handoff into the recursive
	// Run. This closes the window between this dequeue and the recursive
	// Run registering its activeRequests entry: a cancel arriving in
	// that window now records a pending cancel (acceptedRuns > 0) that
	// the recursive Run's accepted path observes as cancel-on-entry.
	first := queuedMessages[0]
	first.Accepted = d.BeginAccepted(sessionID)
	d.messageQueue.Set(sessionID, queuedMessages[1:])
	return queuedMessages, &first, canceledWithRunID
}

// requeueDrained merges a suffix of a previously drained fold batch back
// into the session's queue by accept order. It exists for
// drainQueueForStep's caller: if persisting a folded call fails partway
// through, the calls not yet persisted must not be lost, and they must
// come back in their original accept-sequence position - not just ahead
// of everything - with their original acceptSeq intact, since
// drainQueueForStep never mutates it.
//
// drainQueueForStep only removes the RunID-less calls it folds; any
// RunID-bearing calls interleaved with them stay in the queue (see
// "keep" there). So the queue left behind after a drain, and remainder
// itself, are each already ascending by acceptSeq (the queue's standing
// invariant - the same one drainNext relies on to pop index 0 as the
// earliest-accepted call), and merging them back is a linear merge of
// two sorted runs rather than a blind prepend, or a later RunID-bearing
// call could end up behind a steering message typed after it.
//
// A call with acceptSeq == 0 (an in-process enqueue with no accept
// reservation - see enqueueCall) has no real sequence to compare; it
// sorts first, i.e. as the oldest, matching canceledBySeq's treatment of
// untracked calls as always-covered by a pending cancel. Ties (only
// possible between two untracked calls) favor remainder, since fold
// preserves the front-to-back order those calls held in the queue and
// remainder is drained from further toward the front of that order than
// what's left in existing.
//
// Locking mirrors requeueContinuation.
func (d *dispatcher) requeueDrained(sessionID string, remainder []SessionAgentCall) {
	if len(remainder) == 0 {
		return
	}
	mu := d.sessionMu(sessionID)
	// Registered before the Unlock defer below so it runs after it
	// (defers run LIFO): remainder is non-empty here (checked above), so
	// this always changes the queue.
	defer d.notifyQueueChanged(sessionID)
	mu.Lock()
	defer mu.Unlock()

	existing, _ := d.messageQueue.Get(sessionID)
	merged := make([]SessionAgentCall, 0, len(remainder)+len(existing))
	ri, ei := 0, 0
	for ri < len(remainder) && ei < len(existing) {
		if remainder[ri].acceptSeq <= existing[ei].acceptSeq {
			merged = append(merged, remainder[ri])
			ri++
		} else {
			merged = append(merged, existing[ei])
			ei++
		}
	}
	merged = append(merged, remainder[ri:]...)
	merged = append(merged, existing[ei:]...)
	d.messageQueue.Set(sessionID, merged)
}

// clearPendingCancel removes any pending-cancel mark for sessionID. It
// takes the per-session dispatch lock so it is ordered against Cancel
// and the dispatch handoff.
func (d *dispatcher) clearPendingCancel(sessionID string) {
	mu := d.sessionMu(sessionID)
	mu.Lock()
	defer mu.Unlock()
	d.cancelMark.Del(sessionID)
}

// canceledBySeq reports whether an accepted handle or queued call with
// the given accept sequence is covered by a pending cancel for the
// session. Callers must hold the session's dispatch mutex. A tracked
// sequence (seq > 0) is covered only when it is at or below the cancel
// high-water mark, so a prompt accepted after the cancel (higher seq) is
// never poisoned. An untracked sequence (seq == 0, an in-process enqueue
// with no accept reservation) is covered whenever any mark is present,
// preserving the pre-sequence behavior. The mark is not consumed: it
// stays so every sibling handle it covers observes the same cancel, and
// a later handle (higher seq) ignores it regardless.
func (d *dispatcher) canceledBySeq(sessionID string, seq uint64) bool {
	mark, ok := d.cancelMark.Get(sessionID)
	if !ok || mark == 0 {
		return false
	}
	return seq == 0 || seq <= mark
}

// cancel cancels sessionID's active request (if any), records a pending
// cancel mark covering every currently-accepted-but-not-yet-active run,
// and clears the message queue. It returns the queued calls it dropped so
// the caller can publish their terminal RunComplete - dispatcher itself
// stays free of pubsub.
func (d *dispatcher) cancel(sessionID string) []SessionAgentCall {
	// Serialize against the dispatch handoff in Run so the accepted ->
	// (cancel-on-entry | queued | active) transition is atomic against
	// this cancel. Every cancel observes at least one of: an active
	// request, an accepted run (recorded as a pending cancel), or a
	// queue entry it then clears. If none of those hold, an idle Escape
	// is a true no-op and must not poison the next prompt.
	mu := d.sessionMu(sessionID)
	// changed is set only once the tail below actually clears a
	// non-empty queue. Registered before the Unlock defer so it runs
	// after it (defers run LIFO).
	var changed bool
	defer func() {
		if changed {
			d.notifyQueueChanged(sessionID)
		}
	}()
	mu.Lock()
	defer mu.Unlock()

	// Mark the session canceled unconditionally, before anything below
	// can return early: this is what gates auto-waking a continuation
	// from the completion inbox (see wakeEligible) - "the user canceled
	// the parent session" is recorded by the fact
	// this call happened at all, not by whether it found anything to
	// actually cancel. run() clears it the next time this session's
	// turn genuinely starts.
	d.cancelledSessions.Set(sessionID, struct{}{})

	// Cancel regular requests. Don't use Take() here - we need the entry to
	// remain in activeRequests so IsBusy() returns true until the goroutine
	// fully completes (including error handling that may access the DB).
	// The defer in processRequest will clean up the entry.
	if ac, ok := d.activeRequests.Get(sessionID); ok && ac != nil {
		slog.Debug("Request cancellation initiated", "session_id", sessionID)
		ac.cancel()
	}

	// Record a pending cancel only when a dispatched-but-not-yet-active
	// run exists. This catches runs still in the goroutine scheduler or
	// about to enter Run's busy-queue branch, while leaving an idle
	// session untouched. Active and accepted are not mutually exclusive:
	// when a run is active and a follow-up has been accepted, both the
	// cancel above and this pending record fire.
	//
	// Raise the session's cancel mark to the latest accept sequence
	// assigned so far. Every prompt currently accepted-but-not-yet-
	// active has a sequence at or below that value, so one cancel covers
	// all of them; a prompt accepted after this cancel gets a strictly
	// higher sequence and is never poisoned. Using max keeps repeated
	// cancels idempotent while the same prompts are in flight and lets a
	// later cancel extend coverage to prompts accepted since.
	d.acceptedMu.Lock()
	count, ok := d.acceptedRuns.Get(sessionID)
	mark := d.acceptSeqGen
	d.acceptedMu.Unlock()
	if ok && count > 0 {
		slog.Debug("Recording cancel mark for accepted runs", "session_id", sessionID, "count", count, "mark", mark)
		existing, _ := d.cancelMark.Get(sessionID)
		d.cancelMark.Set(sessionID, max(existing, mark))
	}

	queued, ok := d.messageQueue.Get(sessionID)
	if !ok || len(queued) == 0 {
		return nil
	}
	slog.Debug("Clearing queued prompts", "session_id", sessionID)
	d.messageQueue.Del(sessionID)
	changed = true
	return queued
}

// clearQueue removes all queued prompts for sessionID and returns them so
// the caller can publish a terminal cancelled RunComplete for any that
// carried a RunID.
func (d *dispatcher) clearQueue(sessionID string) []SessionAgentCall {
	mu := d.sessionMu(sessionID)
	// changed is set only once the tail below actually clears a
	// non-empty queue. Registered before the Unlock defer so it runs
	// after it (defers run LIFO).
	var changed bool
	defer func() {
		if changed {
			d.notifyQueueChanged(sessionID)
		}
	}()
	mu.Lock()
	defer mu.Unlock()

	queued, ok := d.messageQueue.Get(sessionID)
	if !ok || len(queued) == 0 {
		return nil
	}
	slog.Debug("Clearing queued prompts", "session_id", sessionID)
	d.messageQueue.Del(sessionID)
	changed = true
	return queued
}

// activeSessionIDs returns the session IDs with a live activeRequests
// entry, for callers (CancelAll) that need to cancel every busy session.
func (d *dispatcher) activeSessionIDs() []string {
	var ids []string
	for key := range d.activeRequests.Seq2() {
		ids = append(ids, key)
	}
	return ids
}

func (d *dispatcher) IsBusy() bool {
	var busy bool
	for ac := range d.activeRequests.Seq() {
		if ac != nil {
			busy = true
			break
		}
	}
	return busy
}

func (d *dispatcher) IsSessionBusy(sessionID string) bool {
	_, busy := d.activeRequests.Get(sessionID)
	return busy
}

func (d *dispatcher) QueuedPrompts(sessionID string) int {
	l, ok := d.messageQueue.Get(sessionID)
	if !ok {
		return 0
	}
	return len(l)
}

func (d *dispatcher) QueuedPromptsList(sessionID string) []string {
	l, ok := d.messageQueue.Get(sessionID)
	if !ok {
		return nil
	}
	prompts := make([]string, len(l))
	for i, call := range l {
		prompts[i] = call.Prompt
	}
	return prompts
}
