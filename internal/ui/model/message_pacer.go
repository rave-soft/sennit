package model

import (
	"sync"
	"time"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
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

// messagePacer collapses the stream of message-update events on its way
// into the program, and passes everything else straight through.
//
// Only pubsub.UpdatedEvent is held back. A create or a delete is a run
// boundary rather than a chunk — rare, and carrying consequences the UI
// acts on immediately (busy state, queue refresh, removing a message) — so
// those are forwarded as they arrive. They also flush whatever is pending
// first, which is what keeps a delete from overtaking an update for the
// same message and resurrecting it.
type messagePacer struct {
	send func(any)

	mu      sync.Mutex
	pending []pubsub.Event[message.Message]
	// index maps a message id to its slot in pending, so a superseded
	// update is replaced where it stands instead of appended. Replacing in
	// place is what keeps the batch in arrival order.
	index   map[string]int
	timer   *time.Timer
	stopped bool
}

// PaceMessages wraps send with a pacer and returns the wrapped function,
// together with a stop function that releases the pending batch and
// prevents any further delivery. The returned send is safe to call from
// one goroutine (the workspace subscription's); the timer flushes on its
// own, under the same lock.
func PaceMessages(send func(any)) (wrapped func(any), stop func()) {
	p := &messagePacer{send: send, index: make(map[string]int)}
	return p.Send, p.Stop
}

// Send is the wrapped delivery function. Message updates accumulate; every
// other message is forwarded immediately, after anything held is released.
func (p *messagePacer) Send(msg any) {
	event, ok := msg.(pubsub.Event[message.Message])
	if !ok || event.Type != pubsub.UpdatedEvent {
		p.flushThen(msg)
		return
	}

	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	if slot, ok := p.index[event.Payload.ID]; ok {
		p.pending[slot] = event
	} else {
		p.index[event.Payload.ID] = len(p.pending)
		p.pending = append(p.pending, event)
	}
	if p.timer == nil {
		p.timer = time.AfterFunc(pacerFrameInterval, p.flush)
	}
	p.mu.Unlock()
}

// flushThen releases the pending batch and then forwards msg, in that
// order, so nothing a later event implies is applied before the updates it
// supersedes.
func (p *messagePacer) flushThen(msg any) {
	p.mu.Lock()
	batch, stopped := p.takeLocked()
	p.mu.Unlock()
	if stopped {
		return
	}
	if len(batch) > 0 {
		p.send(MessagesUpdatedMsg{Events: batch})
	}
	p.send(msg)
}

// flush is the timer's callback.
func (p *messagePacer) flush() {
	p.mu.Lock()
	batch, stopped := p.takeLocked()
	p.mu.Unlock()
	if stopped || len(batch) == 0 {
		return
	}
	p.send(MessagesUpdatedMsg{Events: batch})
}

// takeLocked hands back whatever is pending and disarms the timer.
// Callers hold p.mu.
func (p *messagePacer) takeLocked() (batch []pubsub.Event[message.Message], stopped bool) {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	batch, p.pending = p.pending, nil
	clear(p.index)
	return batch, p.stopped
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
}
