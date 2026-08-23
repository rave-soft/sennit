package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// sessionState is one session's accept/queue/cancel dispatch state: its
// queued follow-up calls, the active run's cancel handle, its completion
// inbox, and the bookkeeping dispatchDecision and Cancel serialize
// against each other. It replaces what used to be seven parallel maps
// (messageQueue, activeRequests, dispatchMu, completionInbox,
// cancelledSessions, acceptedRuns, cancelMark) keyed by session id.
//
// Field locking is split in two, not uniform, because it has to be:
//
//   - mu guards messageQueue, active, completionInbox, and cancelled. It
//     is the per-session "dispatch mutex" dispatchDecision and Cancel
//     serialize the accept -> (cancel-on-entry | queued | active)
//     transition on.
//   - acceptedRuns and cancelMark are guarded by dispatcher.acceptedMu
//     instead — a lock shared by every session, not this one's own mu.
//     dispatchDecision closes an AcceptedRun (which touches
//     acceptedRuns/cancelMark) while it still holds mu; a sync.Mutex is
//     not reentrant, so guarding those two fields with mu would deadlock
//     the very goroutine holding it. Every place that needs both takes
//     mu first and acceptedMu second (never the reverse), so this never
//     becomes a lock-ordering deadlock across goroutines either.
//
// Instances are refcounted and created/removed lazily by
// dispatcher.session/release — see those for the removal invariant that
// fixes the mutex-map's old leak (dispatch.go used to never remove an
// entry once created).
type sessionState struct {
	mu sync.Mutex

	messageQueue    []SessionAgentCall
	active          *activeCancel
	completionInbox []TaskCompletion
	// cancelled mirrors the old cancelledSessions map: "the user
	// explicitly canceled this session" until the next turn actually
	// starts (dispatchDecision's idle branch clears it). It exists
	// solely to gate auto-waking a continuation from the completion
	// inbox — see wakeEligibleLocked.
	cancelled bool

	// acceptedRuns counts dispatched-but-not-yet-active runs for this
	// session. Guarded by dispatcher.acceptedMu, not mu — see the
	// struct doc comment.
	acceptedRuns int
	// cancelMark records a high-water accept sequence: an accepted
	// handle is canceled by it iff the handle's sequence is at or below
	// the mark. 0 means no pending cancel. Guarded by
	// dispatcher.acceptedMu, not mu — see the struct doc comment.
	cancelMark uint64

	// refs is the number of callers currently holding a reference to
	// this state — between a dispatcher.session call and its paired
	// release — whether or not mu happens to be locked at any given
	// instant. Guarded by dispatcher.statesMu, along with creating or
	// removing this session's entry in dispatcher.states. See
	// dispatcher.session for the refcounting invariant this protects.
	refs int
}

// idle reports whether s carries nothing worth keeping once refs drops
// to zero: an empty queue and completion inbox, no active run, no
// cancellation the user is owed, and no accepted-but-not-active run in
// flight. Called only from dispatcher's release, under both statesMu and
// acceptedMu, so the acceptedRuns/cancelMark read here is consistent;
// the other fields are safe to read without mu at that point because
// refs == 0 there means no other caller can be holding or about to lock
// mu (see dispatcher.session).
func (s *sessionState) idle() bool {
	return len(s.messageQueue) == 0 && s.active == nil && len(s.completionInbox) == 0 &&
		!s.cancelled && s.acceptedRuns == 0 && s.cancelMark == 0
}

// dispatcher owns the "accept/queue/cancel" dispatch protocol shared by
// sessionAgent.Run and sessionAgent.Summarize: tracking which sessions are
// active, which prompts have been dispatched but not yet started, and which
// sessions carry a pending cancel that must poison prompts accepted before
// it. It intentionally has no dependency on pubsub or message persistence -
// callers (sessionAgent) are responsible for publishing RunComplete events
// for whatever this type reports as dropped.
type dispatcher struct {
	// states holds one *sessionState per session currently in use. See
	// session/release for the create-on-demand, remove-when-idle
	// lifecycle that replaces the old dispatchMu map's permanent leak.
	states *csync.Map[string, *sessionState]
	// statesMu serializes creating, refcounting, and removing entries in
	// states. It is only ever held briefly (map bookkeeping, no I/O) and
	// is never held while acquiring a *sessionState's own mu or
	// acceptedMu — see session's doc comment for the full ordering
	// argument.
	statesMu sync.Mutex

	// acceptedMu guards every session's acceptedRuns/cancelMark fields
	// and acceptSeqGen below. It is a single lock shared across all
	// sessions (not one per session) specifically so AcceptedRun.Close
	// can run while dispatchDecision holds that session's own mu for an
	// entirely different session without contending on session-specific
	// state - see sessionState's doc comment.
	acceptedMu sync.Mutex
	// acceptSeqGen is the monotonic source of accept sequence numbers.
	// Each BeginAccepted increments it under acceptedMu and stamps the
	// returned handle, so sequences strictly increase in accept order
	// across the agent. Cancel uses its current value as the per-session
	// high-water mark.
	acceptSeqGen uint64

	// delegationParents maps a running delegation's own (child) session
	// id to where its mid-run asks (SendToParent) should be delivered.
	// Registered once, at delegation-create time, by internal/thread -
	// separate from sessionState because a parent target must be
	// resolvable from *inside* the delegation's own turn, on demand,
	// unlike a terminal completion's target, which is resolved once,
	// externally, and handed straight to DeliverTaskCompletion. It also
	// outlives any one turn's accept/queue/cancel bookkeeping for the
	// session's whole lifetime, so it does not belong in the
	// refcounted, idle-reclaimed sessionState. See DelegationParent and
	// RegisterDelegationParent.
	delegationParents *csync.Map[string, DelegationParent]

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

	// userInput holds, per session, a channel closed when a prompt from
	// the user is queued while a turn is already running. Tools that spend
	// a turn waiting on something else (thread_wait) select on it so they
	// can cut the wait short and let the user be answered — see
	// tools.WaitForUserInput. The entry is dropped as it is closed, so the
	// next request arms a fresh one. Kept separate from sessionState: it
	// is its own small, self-contained get-or-create/close-and-delete
	// protocol, unrelated to the accept/queue/cancel state above.
	userInput *csync.Map[string, chan struct{}]
	// userInputMu guards the get-or-create and close-and-delete pair
	// against each other; without it two goroutines can hand out different
	// channels for the same session and one of them never closes.
	userInputMu sync.Mutex
}

// dispatch is a plain alias for dispatcher, used only so sessionAgent can
// embed *dispatcher anonymously while keeping the field's promoted name
// "dispatch" (Go names an embedded field after the identifier written at
// the embed site) - preserving every existing a.X call site
// verbatim while also promoting dispatcher's exported pass-through
// methods (BeginAccepted, IsBusy, IsSessionBusy, QueuedPrompts,
// QueuedPromptsList, RegisterDelegationParent) straight onto
// SessionAgent's method set, with no forwarding wrapper needed.
type dispatch = dispatcher

func newDispatcher() *dispatcher {
	return &dispatcher{
		states:            csync.NewMap[string, *sessionState](),
		userInput:         csync.NewMap[string, chan struct{}](),
		delegationParents: csync.NewMap[string, DelegationParent](),
	}
}

// session returns sessionID's dispatch state, creating it on first use,
// and increments its refcount so it cannot be removed while in use. The
// returned release func must be called exactly once when the caller is
// completely done with the returned state — typically via a `defer`
// registered immediately after this call — regardless of whether, or
// when, the caller locks or unlocks state.mu itself.
//
// Refcounting invariant: refs counts callers currently between session
// and their paired release call for a given session id. Creating an
// entry, mutating refs, and removing an entry are all decided under the
// single statesMu lock, and every accessor in this file (and
// dispatchDecision in agent.go) obtains its reference via session
// before it can touch state.mu or any field guarded by it, releasing
// that reference no earlier than it is done. So a state can never be
// removed out from under a caller currently using it (refs > 0 blocks
// removal), and two callers that need to observe each other's effects
// on the same session always do so through the identical *sessionState
// instance — recreating a fresh one only ever happens after the old one
// was confirmed idle with no outstanding references, at which point a
// zero-valued replacement is behaviorally identical to the one it
// replaced. That is the guarantee dispatchDecision relies on to
// serialize the accept decision against a concurrent Cancel, and it is
// also what makes state removal itself safe: idle() is only trusted at
// refs == 0, when no other goroutine can be holding or about to lock
// mu.
func (d *dispatcher) session(sessionID string) (state *sessionState, release func()) {
	d.statesMu.Lock()
	s, ok := d.states.Get(sessionID)
	if !ok {
		s = &sessionState{}
		d.states.Set(sessionID, s)
	}
	s.refs++
	d.statesMu.Unlock()

	var released bool
	return s, func() {
		if released {
			// Every call site pairs session/release exactly once
			// (normally via a single top-level defer), but guard
			// against a stray double-call anyway: it must not
			// underflow refs and corrupt another session's occupancy
			// accounting.
			return
		}
		released = true
		d.statesMu.Lock()
		s.refs--
		if s.refs == 0 {
			d.acceptedMu.Lock()
			idle := s.idle()
			d.acceptedMu.Unlock()
			if idle {
				// Removing only when the map still points at this exact
				// instance is belt-and-suspenders, not load-bearing: a
				// concurrent session() call for the same id can't
				// interleave here, since both it and this whole release
				// hold statesMu for their entire body.
				if cur, ok := d.states.Get(sessionID); ok && cur == s {
					d.states.Del(sessionID)
				}
			}
		}
		d.statesMu.Unlock()
	}
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

// RegisterDelegationParent records where sessionID (a delegation's own
// child session) should deliver a mid-run ask - see DelegationParent and
// SendToParent. A plain Set: a later registration for the same session
// id simply replaces the earlier one, which is fine since a session only
// ever has one parent for its lifetime.
func (d *dispatcher) RegisterDelegationParent(sessionID string, parent DelegationParent) {
	d.delegationParents.Set(sessionID, parent)
}

// userInputChan returns the channel that closes when the user next sends a
// message to this session, creating it on first use.
func (d *dispatcher) userInputChan(sessionID string) <-chan struct{} {
	d.userInputMu.Lock()
	defer d.userInputMu.Unlock()
	if ch, ok := d.userInput.Get(sessionID); ok {
		return ch
	}
	ch := make(chan struct{})
	d.userInput.Set(sessionID, ch)
	return ch
}

// signalUserInput reports that the user has queued a prompt for this
// session, releasing anything waiting on it. Sessions nobody is waiting on
// have no channel and cost nothing here.
func (d *dispatcher) signalUserInput(sessionID string) {
	d.userInputMu.Lock()
	defer d.userInputMu.Unlock()
	ch, ok := d.userInput.Get(sessionID)
	if !ok {
		return
	}
	d.userInput.Del(sessionID)
	close(ch)
}

// AcceptedRun owns exactly one accept reservation taken by
// BeginAccepted. It is the only carrier of accept-state across the
// AgentDispatcher.run / Coordinator.Run / sessionAgent.Run layers: a
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
	s, release := d.session(sessionID)
	defer release()
	d.acceptedMu.Lock()
	s.acceptedRuns++
	d.acceptSeqGen++
	seq := d.acceptSeqGen
	d.acceptedMu.Unlock()
	return &AcceptedRun{d: d, sessionID: sessionID, seq: seq}
}

// endAccepted decrements the accept counter for sessionID. It is only
// called via AcceptedRun.Close. It uses acceptedMu (not a session's own
// mu) so it can run while dispatchDecision holds that mu for the same
// session without deadlocking — see sessionState's doc comment.
//
// When the count reaches zero the session's cancel mark is dropped: no
// accepted handle remains for it to cover, and any handle accepted later
// gets a strictly higher sequence that the mark would not match anyway.
// Handles canceled on entry never reach RunComplete, so this is the only
// place that clears the mark for an all-canceled batch. Sibling handles
// covered by the same mark are serialized on the session's own mu and
// read the mark before they Close, so this never clears it out from
// under a covered handle still waiting to enter dispatchDecision.
func (d *dispatcher) endAccepted(sessionID string) {
	s, release := d.session(sessionID)
	defer release()
	d.acceptedMu.Lock()
	if s.acceptedRuns <= 1 {
		s.acceptedRuns = 0
		s.cancelMark = 0
	} else {
		s.acceptedRuns--
	}
	d.acceptedMu.Unlock()
}

// canceledBySeq reports whether an accepted handle or queued call with
// the given accept sequence is covered by a pending cancel recorded for
// s. Callers must already hold s.mu — that requirement serializes the
// cancel-on-entry decision against a concurrent enqueue or dequeue on
// the same session; this additionally takes acceptedMu itself to read
// cancelMark, since that field is guarded by acceptedMu, not mu (see
// sessionState's doc comment). A tracked sequence (seq > 0) is covered
// only when it is at or below the cancel high-water mark, so a prompt
// accepted after the cancel (higher seq) is never poisoned. An untracked
// sequence (seq == 0, an in-process enqueue with no accept reservation)
// is covered whenever any mark is present, preserving the
// pre-sequence behavior. The mark is not consumed: it stays so every
// sibling handle it covers observes the same cancel, and a later handle
// (higher seq) ignores it regardless.
func (d *dispatcher) canceledBySeq(s *sessionState, seq uint64) bool {
	d.acceptedMu.Lock()
	mark := s.cancelMark
	d.acceptedMu.Unlock()
	if mark == 0 {
		return false
	}
	return seq == 0 || seq <= mark
}

// notifyQueueChanged invokes onQueueChanged if one is set. Every caller
// must call this only once the per-session dispatch mutex has been
// released: onQueueChanged ultimately reaches a pubsub broker, whose
// subscriber callbacks are unbounded work that must never run under a
// lock documented as covering a brief, I/O-free handoff.
func (d *dispatcher) notifyQueueChanged(sessionID string) {
	if d.onQueueChanged != nil {
		d.onQueueChanged(sessionID)
	}
}

// enqueueLocked appends call to s's message queue. Callers must already
// hold s.mu. It exists separately from enqueueCall so dispatchDecision's
// busy branch — which already holds the session's mu across the whole
// accept decision — can enqueue without relocking a mutex it already
// owns (sync.Mutex is not reentrant).
//
// The OnComplete hook is stripped: the caller that supplied it
// (typically coordinator.Run) has its own retry/coalesce scope that ends
// when it returns, so by the time the queue drains nobody is left to
// consume the buffered terminal event. The recursive Run falls back to
// the default broker publish, which is what existing subscribers expect
// for queued turns.
func enqueueLocked(s *sessionState, call SessionAgentCall) {
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
	s.messageQueue = append(s.messageQueue, queued)
}

// enqueueCall appends call to the session's message queue, acquiring the
// session's dispatch mutex itself. Use this from anywhere that doesn't
// already hold it (tests included); dispatchDecision's busy branch calls
// enqueueLocked directly instead. Unlike drainQueueForStep and friends
// below, this does not call notifyQueueChanged itself: its two callers
// (this file's dispatchDecision-adjacent path is actually enqueueLocked;
// this variant's own callers are external) are responsible for
// notifying once they've dropped the lock.
func (d *dispatcher) enqueueCall(call SessionAgentCall) {
	s, release := d.session(call.SessionID)
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	enqueueLocked(s, call)
}

func (d *dispatcher) requeueContinuation(call SessionAgentCall, onQueued func()) {
	s, release := d.session(call.SessionID)
	defer release()
	// Registered before the lock below so it runs after the unlock
	// (defers run LIFO): this always appends, so the queue always
	// changes.
	defer d.notifyQueueChanged(call.SessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messageQueue = append(s.messageQueue, call)
	onQueued()
}

// drainQueueForStep partitions the session's queued calls for the current
// streaming step under the session's own mutex so the filtering is
// atomic against a concurrent Cancel: canceledBySeq requires the caller to
// hold that mutex, and evaluating it here (rather than after unlocking)
// prevents a cancel recorded between the drain and the check from being
// observed inconsistently.
//
// Calls covered by a pending cancel are dropped; the dropped ones that
// carry a RunID are returned in canceledWithRunID so the caller can
// publish their terminal cancelled RunComplete (a caller waiting on that
// RunID, e.g. `sennit run`, would otherwise hang). Uncanceled calls without
// a RunID are returned in fold to be folded into the active turn,
// preserving the existing follow-up behavior. Uncanceled calls that carry
// a RunID are left in the queue so each runs as its own turn via the
// recursive run path and publishes its own RunComplete, giving every
// RunID-bearing prompt an explicit lifecycle instead of being silently
// absorbed into another turn. fold is processed by the caller without the
// lock held.
func (d *dispatcher) drainQueueForStep(sessionID string) (fold, canceledWithRunID []SessionAgentCall) {
	s, release := d.session(sessionID)
	defer release()
	// Registered before the lock below so it runs after the unlock
	// (defers run LIFO), reading the named returns once they're final.
	// Only notify when something actually left the queue -
	// drainQueueForStep runs on every step of every turn, most of which
	// find nothing queued.
	defer func() {
		if len(fold) > 0 || len(canceledWithRunID) > 0 {
			d.notifyQueueChanged(sessionID)
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()

	var keep []SessionAgentCall
	for _, queued := range s.messageQueue {
		if d.canceledBySeq(s, queued.acceptSeq) {
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
	s.messageQueue = keep
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
	s, release := d.session(sessionID)
	defer release()
	// changed tracks whether the queue held anything to begin with:
	// every branch below either drops entries (cancel-mark filtering) or
	// pops one (the handoff), so a non-empty starting queue always ends
	// up different. Registered before the lock below so it runs after
	// the unlock (defers run LIFO).
	var changed bool
	defer func() {
		if changed {
			d.notifyQueueChanged(sessionID)
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()

	queuedMessages := s.messageQueue
	changed = len(queuedMessages) > 0

	d.acceptedMu.Lock()
	mark := s.cancelMark
	d.acceptedMu.Unlock()

	if mark > 0 && len(queuedMessages) > 0 {
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
		s.messageQueue = kept
	}

	if len(queuedMessages) == 0 {
		// No queued work. Clear the cancel mark only when no accepted
		// run remains in flight that it might still cover; otherwise a
		// sibling prompt (sequence at or below the mark) waiting to
		// enter dispatchDecision would lose its cancellation. When
		// accepted runs are gone, this also clears a stale mark so it
		// can't catch a future run.
		s.messageQueue = nil
		d.acceptedMu.Lock()
		if s.acceptedRuns == 0 {
			s.cancelMark = 0
		}
		d.acceptedMu.Unlock()
		return nil, nil, canceledWithRunID
	}

	// Reserve a fresh accept for the dequeued prompt before dropping the
	// lock so acceptedRuns > 0 across the handoff into the recursive
	// Run. This closes the window between this dequeue and the recursive
	// Run registering its active entry: a cancel arriving in that window
	// now records a pending cancel (acceptedRuns > 0) that the recursive
	// Run's accepted path observes as cancel-on-entry.
	//
	// BeginAccepted re-enters session() for this same id: safe, since it
	// only takes statesMu and acceptedMu, never s.mu, so it cannot
	// deadlock against the s.mu this goroutine already holds.
	first := queuedMessages[0]
	first.Accepted = d.BeginAccepted(sessionID)
	s.messageQueue = queuedMessages[1:]
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
	s, release := d.session(sessionID)
	defer release()
	// Registered before the lock below so it runs after the unlock
	// (defers run LIFO): remainder is non-empty here (checked above), so
	// this always changes the queue.
	defer d.notifyQueueChanged(sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()

	merged := make([]SessionAgentCall, 0, len(remainder)+len(s.messageQueue))
	ri, ei := 0, 0
	for ri < len(remainder) && ei < len(s.messageQueue) {
		if remainder[ri].acceptSeq <= s.messageQueue[ei].acceptSeq {
			merged = append(merged, remainder[ri])
			ri++
		} else {
			merged = append(merged, s.messageQueue[ei])
			ei++
		}
	}
	merged = append(merged, remainder[ri:]...)
	merged = append(merged, s.messageQueue[ei:]...)
	s.messageQueue = merged
}

// cancel cancels sessionID's active request (if any), records a pending
// cancel mark covering every currently-accepted-but-not-yet-active run,
// and clears the message queue. It returns the queued calls it dropped so
// the caller can publish their terminal RunComplete - dispatcher itself
// stays free of pubsub.
func (d *dispatcher) cancel(sessionID string) []SessionAgentCall {
	s, release := d.session(sessionID)
	defer release()
	// changed is set only once the tail below actually clears a
	// non-empty queue. Registered before the lock below so it runs
	// after the unlock (defers run LIFO).
	var changed bool
	defer func() {
		if changed {
			d.notifyQueueChanged(sessionID)
		}
	}()

	// Serialize against the dispatch handoff in dispatchDecision so the
	// accepted -> (cancel-on-entry | queued | active) transition is
	// atomic against this cancel. Every cancel observes at least one of:
	// an active request, an accepted run (recorded as a pending cancel),
	// or a queue entry it then clears. If none of those hold, an idle
	// Escape is a true no-op and must not poison the next prompt.
	s.mu.Lock()
	defer s.mu.Unlock()

	// Mark the session canceled unconditionally, before anything below
	// can return early: this is what gates auto-waking a continuation
	// from the completion inbox (see wakeEligibleLocked) - "the user
	// canceled the parent session" is recorded by the fact this call
	// happened at all, not by whether it found anything to actually
	// cancel. dispatchDecision clears it the next time this session's
	// turn genuinely starts.
	s.cancelled = true

	// Cancel regular requests. active is left in place (not cleared)
	// here - we need it to remain set so IsSessionBusy still reports
	// true until the goroutine fully completes (including error handling
	// that may access the DB). The deferred cleanup in runTurn/finishTurn
	// clears it.
	if s.active != nil {
		slog.Debug("Request cancellation initiated", "session_id", sessionID)
		s.active.cancel()
	}

	// Record a pending cancel only when a dispatched-but-not-yet-active
	// run exists. This catches runs still in the goroutine scheduler or
	// about to enter dispatchDecision's busy-queue branch, while leaving
	// an idle session untouched. Active and accepted are not mutually
	// exclusive: when a run is active and a follow-up has been accepted,
	// both the cancel above and this pending record fire.
	//
	// Raise the session's cancel mark to the latest accept sequence
	// assigned so far. Every prompt currently accepted-but-not-yet-
	// active has a sequence at or below that value, so one cancel covers
	// all of them; a prompt accepted after this cancel gets a strictly
	// higher sequence and is never poisoned. max keeps repeated cancels
	// idempotent while the same prompts are in flight and lets a later
	// cancel extend coverage to prompts accepted since.
	d.acceptedMu.Lock()
	count := s.acceptedRuns
	mark := d.acceptSeqGen
	if count > 0 {
		slog.Debug("Recording cancel mark for accepted runs", "session_id", sessionID, "count", count, "mark", mark)
		s.cancelMark = max(s.cancelMark, mark)
	}
	d.acceptedMu.Unlock()

	if len(s.messageQueue) == 0 {
		return nil
	}
	slog.Debug("Clearing queued prompts", "session_id", sessionID)
	queued := s.messageQueue
	s.messageQueue = nil
	changed = true
	return queued
}

// clearQueue removes all queued prompts for sessionID and returns them so
// the caller can publish a terminal cancelled RunComplete for any that
// carried a RunID.
func (d *dispatcher) clearQueue(sessionID string) []SessionAgentCall {
	s, release := d.session(sessionID)
	defer release()
	// changed is set only once the tail below actually clears a
	// non-empty queue. Registered before the lock below so it runs
	// after the unlock (defers run LIFO).
	var changed bool
	defer func() {
		if changed {
			d.notifyQueueChanged(sessionID)
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.messageQueue) == 0 {
		return nil
	}
	slog.Debug("Clearing queued prompts", "session_id", sessionID)
	queued := s.messageQueue
	s.messageQueue = nil
	changed = true
	return queued
}

// clearActiveIfMatch releases sessionID's active-run slot, but only if it
// still holds ac. This is the compare-and-clear guard a finishing run's
// deferred cleanup needs: it must only remove its own entry, never a
// newer run's entry that was installed in the window between an earlier
// explicit clear and this deferred one. It replaces what used to be a
// csync.CompareAndDelete on a standalone activeRequests map, now
// expressed under the session's own mutex instead of the map's.
func (d *dispatcher) clearActiveIfMatch(sessionID string, ac *activeCancel) {
	s, release := d.session(sessionID)
	defer release()
	s.mu.Lock()
	if s.active == ac {
		s.active = nil
	}
	s.mu.Unlock()
}

// swapActive replaces sessionID's active-run slot from old to new in a
// single locked step, reporting whether the swap happened. It exists so a
// turn handing its slot off to its own summarize call (finishTurn's
// shouldSummarize path) never leaves a window where the session looks
// idle: clearing the old entry and letting summarize claim a fresh one
// as two separate locked steps lets a queued continuation claim the
// session in between, so a turn that should have flowed straight into
// summarize instead fails with ErrSessionBusy. The swap only fails when
// old is no longer installed, which does not happen on the call site
// that uses it today (the caller still owns the slot).
func (d *dispatcher) swapActive(sessionID string, old, newAC *activeCancel) bool {
	s, release := d.session(sessionID)
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != old {
		return false
	}
	s.active = newAC
	return true
}

// activeSessionIDs returns the session IDs with a live active run, for
// callers (CancelAll) that need to cancel every busy session.
func (d *dispatcher) activeSessionIDs() []string {
	var ids []string
	for id, s := range d.states.Seq2() {
		s.mu.Lock()
		busy := s.active != nil
		s.mu.Unlock()
		if busy {
			ids = append(ids, id)
		}
	}
	return ids
}

func (d *dispatcher) IsBusy() bool {
	for _, s := range d.states.Seq2() {
		s.mu.Lock()
		busy := s.active != nil
		s.mu.Unlock()
		if busy {
			return true
		}
	}
	return false
}

func (d *dispatcher) IsSessionBusy(sessionID string) bool {
	s, ok := d.states.Get(sessionID)
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active != nil
}

func (d *dispatcher) QueuedPrompts(sessionID string) int {
	s, ok := d.states.Get(sessionID)
	if !ok {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.messageQueue)
}

func (d *dispatcher) QueuedPromptsList(sessionID string) []string {
	s, ok := d.states.Get(sessionID)
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prompts := make([]string, len(s.messageQueue))
	for i, call := range s.messageQueue {
		prompts[i] = call.Prompt
	}
	return prompts
}

// --- the methods below carry real sessionAgent-level logic (persistence,
// pubsub) that used to live in queue.go as thin pass-throughs onto the
// methods above. They stay on *sessionAgent, not *dispatcher: dispatcher
// itself must stay free of any dependency on pubsub or message
// persistence (see its own doc comment). Every dispatcher method that is
// a pure pass-through (BeginAccepted, IsBusy, IsSessionBusy,
// QueuedPrompts, QueuedPromptsList, RegisterDelegationParent,
// drainQueueForStep, requeueDrained, enqueueCall, and the completion-inbox
// equivalents in completion_inbox.go) needs no such wrapper: sessionAgent
// embeds *dispatcher (via the dispatch alias below) so those are promoted
// straight onto SessionAgent's method set.

// publishCanceledQueueDrops emits a terminal cancelled RunComplete for
// every dropped queued call that carries a RunID. A queued prompt removed
// from the queue without ever running — covered by a pending cancel, or
// cleared by Cancel/ClearQueue — would otherwise leave a caller blocked on
// that RunID: `sennit run` ignores live message events and exits only on a
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

// DeliverTaskCompletion enqueues completion into sessionID's completion
// inbox and, if that leaves the session eligible (idle, not left
// canceled by the user), attempts a continuation turn for it. See
// dispatcher.enqueueCompletion and startContinuation. The attempt does
// not itself drain anything - the completion stays in the inbox until
// whichever turn actually becomes active drains it via PrepareStep, so a
// continuation call that loses its own busy-check race (see
// dispatchDecision) drops cleanly without losing or duplicating
// completion's content.
func (a *sessionAgent) DeliverTaskCompletion(ctx context.Context, sessionID string, completion TaskCompletion) {
	if !a.enqueueCompletion(sessionID, completion) {
		return
	}
	a.startContinuation(ctx, sessionID, "completion arrived while session was idle")
}

// SendToParent delivers a mid-run ask from sessionID to its registered
// parent, riding the exact same delivery path DeliverTaskCompletion
// uses (enqueue, at-most-once, idle-wake) via the registered parent's
// own Coordinator - never this sessionAgent's, since a thread's parent
// may live in an entirely different Coordinator/App. Non-blocking: it
// enqueues and returns without waiting for a reply, exactly like
// DeliverTaskCompletion.
func (a *sessionAgent) SendToParent(ctx context.Context, sessionID, msg string) error {
	parent, ok := a.delegationParents.Get(sessionID)
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
		Message:        msg,
	})
	return nil
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

// Cancel cancels sessionID's active run (if any) and any accepted or
// queued follow-ups. See dispatcher.cancel.
func (a *sessionAgent) Cancel(sessionID string) {
	drops := a.cancel(sessionID)
	a.publishCanceledQueueDrops(drops)
}

// ClearQueue drops sessionID's queued follow-ups without touching the
// active run or any pending cancel mark. See dispatcher.clearQueue.
func (a *sessionAgent) ClearQueue(sessionID string) {
	drops := a.clearQueue(sessionID)
	a.publishCanceledQueueDrops(drops)
}

func (a *sessionAgent) CancelAll() {
	if !a.IsBusy() {
		return
	}
	for _, sessionID := range a.activeSessionIDs() {
		a.Cancel(sessionID)
	}

	// Poll IsBusy on a short tick until every canceled run's cleanup has
	// run, bounded by the same 5s budget as before. There is no
	// broadcast/wait primitive for "a run just finished" (adding one to
	// dispatcher for this single caller would be a bigger change than
	// this polling loop justifies). A 10ms tick keeps this from being a
	// coarse 200ms busy-wait while staying a minimal, local change.
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
