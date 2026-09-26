package grpcws

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
)

// defaultEventBufferSize is eventHub's ring buffer capacity
// (CLIENT-SERVER.md, PR 1.2 build step 1) when WithEventBufferSize isn't
// given: generous enough that a client reconnecting after a short network
// blip replays instead of resyncing.
const defaultEventBufferSize = 4096

// hubEvent is one ring buffer slot: env is the already-EncodeEvent'd
// payload, seq its position in this hub's stream.
type hubEvent struct {
	seq uint64
	env wsrpc.Envelope
}

// eventHub fans out one served workspace's events (root handle only for
// now -- see NewServer) to every live Subscribe stream, keeping the last
// capacity of them so a client that reconnects with FromSeq can replay
// instead of losing events. It has exactly one upstream subscription
// (ws.SubscribeWith, started lazily by ensureStarted) regardless of how
// many gRPC streams are attached, so N reconnecting clients cost one
// upstream subscription, not N.
type eventHub struct {
	capacity int

	startOnce sync.Once

	mu      sync.Mutex
	stop    func() // ws.SubscribeWith's stop func, once startOnce has run; nil until then. Guarded by mu (not just startOnce) so close, on a different goroutine than ensureStarted, has a happens-before edge to read it.
	nextSeq uint64 // seq to assign to the next published event; starts at 1.
	buf     []hubEvent
	subs    map[*hubSubscriber]struct{}
}

func newEventHub(capacity int) *eventHub {
	if capacity <= 0 {
		capacity = defaultEventBufferSize
	}
	return &eventHub{capacity: capacity, nextSeq: 1, subs: map[*hubSubscriber]struct{}{}}
}

// ensureStarted subscribes to ws exactly once, the first time any stream
// needs this hub. The callback recovers its own panics (an encoding bug,
// or anything else unexpected in publish) instead of letting them escape
// into ws.SubscribeWith's own goroutine, whose sole recover just ends the
// whole upstream subscription -- see AppWorkspace.SubscribeWith and
// CLIENT-SERVER.md's PR 1.2 build step 3 for why that would take every
// stream on this hub down at once, a bigger blast radius than "one
// stream".
func (h *eventHub) ensureStarted(ws subscriber) {
	h.startOnce.Do(func() {
		stop := ws.SubscribeWith(func(v any) {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("Wsrpc event hub subscriber callback panicked, dropping event", "panic", r)
				}
			}()
			h.publish(v)
		})
		h.mu.Lock()
		h.stop = stop
		h.mu.Unlock()
	})
}

// subscriber is the slice of workspace.Workspace the hub needs; declared
// locally so this file doesn't have to import the (larger) workspace
// package just to name SubscribeWith's signature.
type subscriber interface {
	SubscribeWith(send func(any)) (stop func())
}

// close stops the upstream subscription, if one was ever started. Safe to
// call even when ensureStarted never ran.
func (h *eventHub) close() {
	h.mu.Lock()
	stop := h.stop
	h.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// publish encodes v through wsrpc's event registry, assigns it the next
// Seq, stores it in the ring buffer, and fans it out to every live
// subscriber -- all under the hub lock except the actual per-subscriber
// delivery, which is why deliver must never block (see hubSubscriber).
// An encode error (v is not a registered event type) is logged and the
// event is dropped rather than crashing the hub.
func (h *eventHub) publish(v any) {
	env, err := wsrpc.EncodeEvent(v)
	if err != nil {
		slog.Error("Wsrpc event hub failed to encode event, dropping it", "type", fmt.Sprintf("%T", v), "error", err)
		return
	}

	h.mu.Lock()
	seq := h.nextSeq
	h.nextSeq++
	h.appendLocked(hubEvent{seq: seq, env: env})
	subs := make([]*hubSubscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()

	frame := EventFrame{Seq: seq, Event: &env}
	for _, s := range subs {
		s.deliver(frame)
	}
}

// appendLocked adds ev to the ring buffer, dropping the oldest entry once
// capacity is reached. Called with h.mu held.
func (h *eventHub) appendLocked(ev hubEvent) {
	if len(h.buf) < h.capacity {
		h.buf = append(h.buf, ev)
		return
	}
	copy(h.buf, h.buf[1:])
	h.buf[len(h.buf)-1] = ev
}

// oldestSeqLocked is the lowest Seq a replay can start from. When nothing
// has been buffered yet (including "never published anything"), that's
// nextSeq itself: there is no gap to resync over, because nothing has
// been dropped. Called with h.mu held.
func (h *eventHub) oldestSeqLocked() uint64 {
	if len(h.buf) > 0 {
		return h.buf[0].seq
	}
	return h.nextSeq
}

// subscribe registers a new subscriber and reports what a Subscribe RPC
// handler should do with it (CLIENT-SERVER.md, PR 1.2 build step 2):
//   - fromSeq == 0: live only, no replay, no resync.
//   - fromSeq within [oldest buffered, nextSeq]: replay is every buffered
//     event from fromSeq onward (possibly none, when fromSeq == nextSeq),
//     then continue live.
//   - fromSeq outside that range (too old, buffer no longer has it; or
//     newer than anything ever produced): resync, then continue live.
//
// The subscriber is added to h.subs before any of this is decided, under
// the same lock, so no event published concurrently with a reconnecting
// client's Subscribe call is ever missed between "read the buffer" and
// "start listening live".
func (h *eventHub) subscribe(fromSeq uint64) (sub *hubSubscriber, replay []hubEvent, resync bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	sub = newHubSubscriber(h.capacity)
	h.subs[sub] = struct{}{}

	if fromSeq == 0 {
		return sub, nil, false
	}
	if fromSeq < h.oldestSeqLocked() || fromSeq > h.nextSeq {
		return sub, nil, true
	}
	for _, e := range h.buf {
		if e.seq >= fromSeq {
			replay = append(replay, e)
		}
	}
	return sub, replay, false
}

// unsubscribe removes sub from the fan-out set. Safe to call more than
// once (e.g. from both a stream handler's defer and deliver's own
// overflow path).
func (h *eventHub) unsubscribe(sub *hubSubscriber) {
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
	sub.close()
}

// hubSubscriber is one live Subscribe stream's inbox: a bounded channel
// the hub delivers into and the stream handler drains. Guarded by its own
// mutex (not the hub's) so a slow subscriber's overflow handling never
// contends with the hub publishing to other subscribers.
type hubSubscriber struct {
	ch chan EventFrame

	mu     sync.Mutex
	closed bool
}

func newHubSubscriber(capacity int) *hubSubscriber {
	return &hubSubscriber{ch: make(chan EventFrame, capacity)}
}

// deliver is always non-blocking, which is what keeps one saturated
// subscriber from slowing the hub or any other subscriber (CLIENT-SERVER.md,
// PR 1.2 build step 1). A full channel means this subscriber fell behind
// by more than its queue: it is told to resync (best effort -- if even
// that doesn't fit, the stream just ends, and the client's own reconnect
// discovers the gap via FromSeq) and its channel is closed, ending its
// stream without touching anyone else's.
func (s *hubSubscriber) deliver(frame EventFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- frame:
		return
	default:
	}
	select {
	case s.ch <- EventFrame{Resync: true}:
	default:
	}
	s.closed = true
	close(s.ch)
}

// close is deliver's overflow path, exposed so unsubscribe can also tear
// a subscriber down on the normal "client disconnected" path.
func (s *hubSubscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
}
