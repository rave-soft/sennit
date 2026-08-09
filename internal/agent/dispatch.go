package agent

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/rave-soft/braid/internal/csync"
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
}

func newDispatcher() *dispatcher {
	return &dispatcher{
		messageQueue:   csync.NewMap[string, []SessionAgentCall](),
		activeRequests: csync.NewMap[string, *activeCancel](),
		dispatchMu:     csync.NewMap[string, *sync.Mutex](),
		acceptedRuns:   csync.NewMap[string, int](),
		cancelMark:     csync.NewMap[string, uint64](),
	}
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

// enqueueCall appends call to the session's message queue. The
// OnComplete hook is stripped: the caller that supplied it (typically
// coordinator.Run) has its own retry/coalesce scope that ends when it
// returns, so by the time the queue drains nobody is left to consume the
// buffered terminal event. The recursive Run falls back to the default
// broker publish, which is what existing subscribers expect for queued
// turns.
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
	existing = append(existing, queued)
	d.messageQueue.Set(call.SessionID, existing)
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
	mu.Lock()
	defer mu.Unlock()

	queuedMessages, _ := d.messageQueue.Get(sessionID)
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
	mu.Lock()
	defer mu.Unlock()

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
	return queued
}

// clearQueue removes all queued prompts for sessionID and returns them so
// the caller can publish a terminal cancelled RunComplete for any that
// carried a RunID.
func (d *dispatcher) clearQueue(sessionID string) []SessionAgentCall {
	queued, ok := d.messageQueue.Get(sessionID)
	if !ok || len(queued) == 0 {
		return nil
	}
	slog.Debug("Clearing queued prompts", "session_id", sessionID)
	d.messageQueue.Del(sessionID)
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
