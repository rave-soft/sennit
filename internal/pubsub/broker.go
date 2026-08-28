// Package pubsub provides a lightweight in-process broker for fan-out
// event delivery between services and the UI.
//
// Delivery semantics:
//
//   - [Broker.Publish] is best-effort and lossy under contention. If a
//     subscriber's channel is full, the event is dropped for that
//     subscriber, a warning is logged, and a counter is incremented.
//     This is the right choice for high-frequency intermediate updates
//     (e.g. streaming token deltas) where only the latest state
//     matters.
//
//   - [Broker.PublishMustDeliver] is bounded-blocking. For each
//     subscriber it first tries a non-blocking send, then falls back to
//     a blocking send with a hard timeout. All subscribers are served
//     concurrently, so the timeout is a bound on the whole fan-out, not
//     on each subscriber individually — N saturated subscribers cost
//     one timeout window, not N. On timeout the event is dropped for
//     that subscriber, an error is logged, and the must-deliver drop
//     counter is incremented. The publisher never blocks indefinitely.
//     This is the right choice for terminal events (finish, tool
//     result, error, cancel) that must not be silently coalesced away.
//
// Drop counters ([Broker.DropCount], [Broker.MustDeliverDropCount]) are
// exposed so callers can surface saturation in telemetry.
package pubsub

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// bufferSize is the per-subscriber channel capacity for any broker
	// created via NewBroker. Publish is non-blocking, so a full buffer
	// drops events (with a warning log); sized to cover a long
	// streaming assistant turn (~one UpdatedEvent per token) even under
	// TUI render stalls.
	bufferSize = 4096

	// defaultMustDeliverTimeout is the per-subscriber upper bound on how
	// long [Broker.PublishMustDeliver] will block waiting for buffer
	// space before giving up on that subscriber.
	defaultMustDeliverTimeout = 50 * time.Millisecond
)

// subscription owns one subscriber's channel plus the lock that
// serializes sends against close. Sends take subMu for reading, close
// takes it for writing; the closed flag (only ever read/written under
// subMu) lets a send that acquires the lock after close has already run
// notice the channel is gone instead of sending on a closed channel.
//
// This lock is scoped to a single subscriber, not the whole broker, so a
// slow or blocking send to one subscriber (see [Broker.PublishMustDeliver])
// only ever delays that subscriber's own unsubscribe — it never blocks
// [Broker.Subscribe] or any other subscriber's teardown, which only need
// the broker's map lock.
type subscription[T any] struct {
	ch     chan Event[T]
	subMu  sync.RWMutex
	closed bool
}

// deliverResult reports what happened when a subscription attempted to
// deliver an event.
type deliverResult int

const (
	deliverOK       deliverResult = iota
	deliverGone                   // subscription already closed; not a drop.
	deliverFull                   // buffer was full; a drop.
	deliverTimedOut               // blocking send exceeded the timeout; a drop.
	deliverCanceled               // caller's context was canceled mid-send.
)

// send attempts a non-blocking delivery, used by the lossy [Broker.Publish].
func (s *subscription[T]) send(event Event[T]) deliverResult {
	s.subMu.RLock()
	defer s.subMu.RUnlock()
	if s.closed {
		return deliverGone
	}
	select {
	case s.ch <- event:
		return deliverOK
	default:
		return deliverFull
	}
}

// sendMustDeliver attempts a non-blocking send, then falls back to a
// blocking send bounded by timeout. Holding subMu for the duration of the
// blocking wait is what makes this subscriber's own close wait for the
// send to finish rather than racing it (see close); it does not affect
// any other subscription.
func (s *subscription[T]) sendMustDeliver(ctx context.Context, event Event[T], timeout time.Duration) deliverResult {
	s.subMu.RLock()
	defer s.subMu.RUnlock()
	if s.closed {
		return deliverGone
	}
	select {
	case s.ch <- event:
		return deliverOK
	default:
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case s.ch <- event:
		return deliverOK
	case <-timer.C:
		return deliverTimedOut
	case <-ctx.Done():
		return deliverCanceled
	}
}

// close marks the subscription closed and closes its channel, unless
// already closed. It takes subMu for writing, so it waits for any send
// currently in flight (bounded by that send's own timeout) and, once it
// runs, guarantees no later send observes an open, unclosed-looking
// channel: send() and sendMustDeliver() both check closed under the same
// lock before touching the channel.
func (s *subscription[T]) close() {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
}

type Broker[T any] struct {
	subs                 map[*subscription[T]]struct{}
	mu                   sync.RWMutex
	done                 chan struct{}
	shutdownOnce         sync.Once
	subCount             int
	channelBufferSize    int
	mustDeliverTimeout   time.Duration
	dropCount            atomic.Uint64
	mustDeliverDropCount atomic.Uint64
}

func NewBroker[T any]() *Broker[T] {
	return NewBrokerWithOptions[T](bufferSize)
}

func NewBrokerWithOptions[T any](channelBufferSize int) *Broker[T] {
	return &Broker[T]{
		subs:               make(map[*subscription[T]]struct{}),
		done:               make(chan struct{}),
		channelBufferSize:  channelBufferSize,
		mustDeliverTimeout: defaultMustDeliverTimeout,
	}
}

// SetMustDeliverTimeout overrides the per-subscriber timeout used by
// [Broker.PublishMustDeliver]. A zero or negative value resets to the
// default. Intended primarily for tests.
func (b *Broker[T]) SetMustDeliverTimeout(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d <= 0 {
		b.mustDeliverTimeout = defaultMustDeliverTimeout
		return
	}
	b.mustDeliverTimeout = d
}

func (b *Broker[T]) Shutdown() {
	b.shutdownOnce.Do(func() {
		close(b.done)

		b.mu.Lock()
		subs := make([]*subscription[T], 0, len(b.subs))
		for sub := range b.subs {
			subs = append(subs, sub)
		}
		b.subs = make(map[*subscription[T]]struct{})
		b.subCount = 0
		b.mu.Unlock()

		// Close outside the map lock: close() may briefly wait for an
		// in-flight send to that specific subscription to finish, and
		// doing that under b.mu would once again let one slow
		// subscriber stall everyone else — the exact problem this
		// refactor removes.
		for _, sub := range subs {
			sub.close()
		}
	})
}

func (b *Broker[T]) Subscribe(ctx context.Context) <-chan Event[T] {
	b.mu.Lock()
	defer b.mu.Unlock()

	select {
	case <-b.done:
		ch := make(chan Event[T])
		close(ch)
		return ch
	default:
	}

	sub := &subscription[T]{ch: make(chan Event[T], b.channelBufferSize)}
	b.subs[sub] = struct{}{}
	b.subCount++

	go func() {
		// Wake on either the caller's context or broker shutdown.
		// Without the b.done case, a subscriber whose ctx outlives the
		// broker (e.g. a background context) would leak this goroutine
		// forever after Shutdown, since Shutdown closes the channel
		// itself without signaling this goroutine.
		select {
		case <-ctx.Done():
		case <-b.done:
			return
		}

		b.mu.Lock()
		_, ok := b.subs[sub]
		if ok {
			delete(b.subs, sub)
			b.subCount--
		}
		b.mu.Unlock()

		if ok {
			sub.close()
		}
		// If !ok, Shutdown already removed and closed this subscription.
	}()

	return sub.ch
}

func (b *Broker[T]) GetSubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.subCount
}

// DropCount returns the cumulative number of events dropped by
// [Broker.Publish] because a subscriber's channel was full.
func (b *Broker[T]) DropCount() uint64 {
	return b.dropCount.Load()
}

// MustDeliverDropCount returns the cumulative number of events dropped
// by [Broker.PublishMustDeliver] after the per-subscriber timeout
// expired.
func (b *Broker[T]) MustDeliverDropCount() uint64 {
	return b.mustDeliverDropCount.Load()
}

// snapshot returns the current subscriptions under the map lock, then
// releases it. Delivery happens afterward, outside the lock, so a slow
// or blocking send never holds up [Broker.Subscribe] or unsubscription.
func (b *Broker[T]) snapshot() []*subscription[T] {
	b.mu.RLock()
	defer b.mu.RUnlock()
	subs := make([]*subscription[T], 0, len(b.subs))
	for sub := range b.subs {
		subs = append(subs, sub)
	}
	return subs
}

// Publish delivers an event to every active subscriber.
//
// Delivery is non-blocking and lossy: if a subscriber's channel is full
// the event is dropped for that subscriber, a warning is logged, and
// [Broker.DropCount] is incremented. Use [Broker.PublishMustDeliver]
// for events that must not be silently dropped.
func (b *Broker[T]) Publish(t EventType, payload T) {
	select {
	case <-b.done:
		return
	default:
	}

	subs := b.snapshot()
	event := Event[T]{Type: t, Payload: payload}

	for _, sub := range subs {
		switch sub.send(event) {
		case deliverOK, deliverGone:
			// Delivered, or the subscriber already unsubscribed — a
			// subscription that's gone simply has nothing to deliver
			// to, same as if it weren't in the snapshot at all.
		case deliverFull:
			// Lossy by design; counted and logged so saturation is
			// observable.
			b.dropCount.Add(1)
			slog.Warn("Pubsub buffer full; dropping event", "type", t)
		}
	}
}

// PublishMustDeliver delivers an event with bounded-blocking semantics.
// For each subscriber it first attempts a non-blocking send, then falls
// back to a blocking send bounded by a timeout (default
// [defaultMustDeliverTimeout]). On timeout the event is dropped for
// that subscriber, [Broker.MustDeliverDropCount] is incremented, and an
// error is logged. The publisher never blocks indefinitely.
//
// Subscribers are served concurrently: each gets its own goroutine
// racing sendMustDeliver's own timer, and PublishMustDeliver waits for
// all of them before returning. Since every goroutine starts at
// essentially the same instant, those independently-started timers
// expire together and act as one shared deadline for the whole
// fan-out — a derived context.WithTimeout would express that same
// deadline but would also route a subscriber's per-send timeout through
// ctx.Done(), turning deliverTimedOut into deliverCanceled and losing
// the distinction the counters and logs rely on. With one subscriber
// there is nothing to fan out, so the send happens inline and skips the
// goroutine and WaitGroup entirely.
//
// If the caller's ctx is canceled mid-flight, every subscriber still
// waiting on its blocking send observes that independently and stops
// there — no drop is counted or logged for those, since the failure is
// the caller's, not the subscriber's. This differs from the sequential
// version: canceling used to also skip subscribers whose turn hadn't
// come up yet (not even the non-blocking attempt), whereas now every
// subscriber has already been offered the non-blocking send, so
// cancellation can only affect ones that had to fall back to blocking.
//
// Use this for terminal events that must reach subscribers (finish,
// tool result, error, cancel). Callers must still tolerate rare drops
// after timeout — recovery is the subscriber's responsibility (e.g. a
// re-fetch on the next session-visible event).
func (b *Broker[T]) PublishMustDeliver(ctx context.Context, t EventType, payload T) {
	select {
	case <-b.done:
		return
	default:
	}

	b.mu.RLock()
	timeout := b.mustDeliverTimeout
	b.mu.RUnlock()

	subs := b.snapshot()
	event := Event[T]{Type: t, Payload: payload}

	if len(subs) == 1 {
		b.recordMustDeliverResult(subs[0].sendMustDeliver(ctx, event, timeout), t, timeout)
		return
	}

	var wg sync.WaitGroup
	for _, sub := range subs {
		wg.Go(func() {
			b.recordMustDeliverResult(sub.sendMustDeliver(ctx, event, timeout), t, timeout)
		})
	}
	wg.Wait()
}

// recordMustDeliverResult applies the counter/log side effects of one
// subscriber's delivery outcome. Split out of PublishMustDeliver so the
// single-subscriber fast path and the fanned-out goroutines share the
// same bookkeeping; mustDeliverDropCount.Add is safe to call
// concurrently from those goroutines since it's an atomic counter.
func (b *Broker[T]) recordMustDeliverResult(result deliverResult, t EventType, timeout time.Duration) {
	switch result {
	case deliverOK, deliverGone:
		// Delivered, or the subscriber is already gone — either way
		// there's nothing more to do for it.
	case deliverTimedOut:
		b.mustDeliverDropCount.Add(1)
		slog.Error("PublishMustDeliver timed out delivering event",
			"type", t, "timeout", timeout)
	case deliverCanceled:
		// Caller gave up; no drop is counted or logged since this
		// isn't a subscriber-side saturation event.
	}
}
