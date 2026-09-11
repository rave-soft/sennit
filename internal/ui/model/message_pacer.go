package model

import (
	"sync"
	"time"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
)

// MessagesUpdatedMsg carries one frame's worth of streaming message
// updates, already collapsed to at most one event per message.
//
// It exists because bubbletea calls Model.View() — a full re-layout of the
// whole UI — once per message delivered to Update, while its renderer
// flushes to the terminal at most 60 times a second. Forwarding every
// message update on its own therefore bought nothing above that rate and
// cost a re-layout each time. The message store debounces its writes per
// message (see internal/message/store, defaultUpdateDebounce), so each
// streaming assistant message published ~30 updates a second on a clock of
// its own: a dozen delegations running at once put several hundred
// re-layouts a second on the UI thread, most of them overwritten before
// anything was drawn.
//
// Batching turns that into one message per frame however many sessions are
// streaming. The events inside are handled exactly as they would have been
// individually, in the order they arrived.
type MessagesUpdatedMsg struct {
	Events []pubsub.Event[message.Message]
}

// pacerFrameInterval is how long updates are held before a batch is sent.
// It matches bubbletea's own 60fps render ceiling: holding them longer
// would be visible, holding them for less would produce frames the
// renderer discards.
const pacerFrameInterval = time.Second / 60

// messagePacer collapses the stream of message- and session-update events
// on its way into the program, and passes everything else straight through.
//
// Only pubsub.UpdatedEvent is held back. A create or a delete is a run
// boundary rather than a chunk — rare, and carrying consequences the UI
// acts on immediately (busy state, queue refresh, removing a message) — so
// those are forwarded as they arrive. They also flush whatever is pending
// first, which is what keeps a delete from overtaking an update for the
// same message and resurrecting it.
//
// Session updates are paced for the same reason as message updates, and
// pacing them is what makes pacing messages work at all. The session store
// publishes on every write with no debounce of its own (unlike the message
// store's, see internal/message/store defaultUpdateDebounce), and each
// publish re-reads the session from SQLite to pick up title and usage
// changes (internal/session/store.publishSessionUpdate). A streaming turn
// therefore emits session updates at least as fast as message updates. Left
// unpaced they took the flushThen path below, so every one of them released
// the message batch early and then laid the UI out a second time for itself:
// two full re-layouts per update, on a pacer whose whole purpose was to cap
// that at one per frame.
type messagePacer struct {
	send func(any)

	mu      sync.Mutex
	pending []pubsub.Event[message.Message]
	// index maps a message id to its slot in pending, so a superseded
	// update is replaced where it stands instead of appended. Replacing in
	// place is what keeps the batch in arrival order.
	index map[string]int
	// sessionPending and sessionIndex are the same one-slot-per-id
	// arrangement for session updates. They are kept apart from the message
	// batch because they are released as the individual events the UI
	// already handles, rather than collapsed into one message of their own.
	sessionPending []pubsub.Event[session.Session]
	sessionIndex   map[string]int
	timer          *time.Timer
	stopped        bool
	// delivering says a goroutine is inside deliver, handing batches to the
	// program. At most one ever is, and that is the pacer's backpressure:
	// the program's Send blocks once its message queue is full, and a
	// blocked delivery must turn incoming load into more coalescing rather
	// than into more goroutines waiting their turn to push the same
	// superseded updates.
	//
	// Without it the timer did exactly that. takeLocked clears p.timer, so
	// a delivery that blocked in Send left the pacer able to arm another
	// one a frame later, and another, each on a goroutine of its own. A
	// wedged UI was found holding 922 of them parked in Send — a queue of
	// work that was, by then, mostly updates newer arrivals had already
	// replaced.
	delivering bool
	// idle is broadcast when delivering goes back to false, so a caller
	// that must deliver in order (see flushThen) can wait for its turn
	// instead of racing the timer.
	idle *sync.Cond
}

// PaceMessages wraps send with a pacer and returns the wrapped function,
// together with a stop function that releases the pending batch and
// prevents any further delivery. The returned send is safe to call from
// one goroutine (the workspace subscription's); the timer flushes on its
// own, under the same lock.
func PaceMessages(send func(any)) (wrapped func(any), stop func()) {
	p := &messagePacer{
		send:         send,
		index:        make(map[string]int),
		sessionIndex: make(map[string]int),
	}
	p.idle = sync.NewCond(&p.mu)
	return p.Send, p.Stop
}

// Send is the wrapped delivery function. Message and session updates
// accumulate; every other message is forwarded immediately, after anything
// held is released.
func (p *messagePacer) Send(msg any) {
	switch event := msg.(type) {
	case pubsub.Event[message.Message]:
		if event.Type == pubsub.UpdatedEvent {
			p.holdMessage(event)
			return
		}
	case pubsub.Event[session.Session]:
		if event.Type == pubsub.UpdatedEvent {
			p.holdSession(event)
			return
		}
	}
	p.flushThen(msg)
}

// holdMessage adds event to the pending batch, replacing any earlier update
// for the same message, and arms the frame timer if it isn't already.
func (p *messagePacer) holdMessage(event pubsub.Event[message.Message]) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	if slot, ok := p.index[event.Payload.ID]; ok {
		p.pending[slot] = event
	} else {
		p.index[event.Payload.ID] = len(p.pending)
		p.pending = append(p.pending, event)
	}
	p.armLocked()
}

// holdSession is holdMessage for session updates; see the type's comment on
// why they are paced at all.
func (p *messagePacer) holdSession(event pubsub.Event[session.Session]) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	if slot, ok := p.sessionIndex[event.Payload.ID]; ok {
		p.sessionPending[slot] = event
	} else {
		p.sessionIndex[event.Payload.ID] = len(p.sessionPending)
		p.sessionPending = append(p.sessionPending, event)
	}
	p.armLocked()
}

// armLocked starts the frame timer unless one is already running, or a
// delivery is in flight — that one drains whatever arrives before it
// finishes, so a timer for this event would only add a goroutine that
// waits for it and then finds nothing to do. Callers hold p.mu.
func (p *messagePacer) armLocked() {
	if p.timer == nil && !p.delivering {
		p.timer = time.AfterFunc(pacerFrameInterval, p.flush)
	}
}

// flushThen releases the pending batch and then forwards msg, in that
// order, so nothing a later event implies is applied before the updates it
// supersedes.
func (p *messagePacer) flushThen(msg any) {
	if stopped := p.deliver(); stopped {
		return
	}
	p.send(msg)
}

// flush is the timer's callback.
func (p *messagePacer) flush() {
	p.deliver()
}

// deliver hands held events to the program until nothing is left, and
// reports whether the pacer was stopped. Only one goroutine runs it at a
// time; a second waits for the first to finish rather than pushing a
// second batch of its own, so however long the program takes to accept a
// message, the pacer answers with one goroutine and a fuller batch instead
// of many goroutines and the same work split between them.
//
// The loop re-reads what is pending after every send precisely so that
// everything arriving while the program was busy is picked up here, by
// this goroutine, already collapsed.
func (p *messagePacer) deliver() (stopped bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.delivering {
		if p.stopped {
			return true
		}
		p.idle.Wait()
	}
	p.delivering = true
	for {
		batch, sessions, stopped := p.takeLocked()
		if stopped || (len(batch) == 0 && len(sessions) == 0) {
			p.delivering = false
			p.idle.Broadcast()
			return stopped
		}
		p.mu.Unlock()
		p.release(batch, sessions)
		p.mu.Lock()
	}
}

// release delivers one frame's held events: the message batch first, then
// the session updates in arrival order. That order matters — a session
// update reports totals derived from the messages of the same turn, so
// applying it before them would show the UI a count for content it has not
// been given yet.
func (p *messagePacer) release(batch []pubsub.Event[message.Message], sessions []pubsub.Event[session.Session]) {
	if len(batch) > 0 {
		p.send(MessagesUpdatedMsg{Events: batch})
	}
	for _, event := range sessions {
		p.send(event)
	}
}

// takeLocked hands back whatever is pending and disarms the timer.
// Callers hold p.mu.
func (p *messagePacer) takeLocked() (batch []pubsub.Event[message.Message], sessions []pubsub.Event[session.Session], stopped bool) {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	batch, p.pending = p.pending, nil
	clear(p.index)
	sessions, p.sessionPending = p.sessionPending, nil
	clear(p.sessionIndex)
	return batch, sessions, p.stopped
}

// Stop drops anything still held and makes every later Send a no-op, so a
// batch cannot be delivered to a program that has finished running.
func (p *messagePacer) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.pending = nil
	clear(p.index)
	p.sessionPending = nil
	clear(p.sessionIndex)
	// Wake anyone waiting for a delivery that is no longer going to
	// produce anything, so Stop cannot leave a goroutine parked for the
	// life of the process.
	p.idle.Broadcast()
}
