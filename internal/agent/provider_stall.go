package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// A provider stream that goes quiet has no natural end. The HTTP request
// is open, the connection is healthy as far as the transport can tell,
// and nothing on either side is obliged to speak first — so the read
// blocks, the turn blocks behind it, and the run never terminates. There
// is no completion event, which means for a delegated run there is also
// no terminal status: internal/thread only ever learns a task ended by
// observing the run that ends it. The task then sits at StatusRunning
// forever, holding one of the concurrency slots
// (thread.maxActiveTasksPerParentTurn) that a later delegation needs, and
// the parent agent is never told the work it is waiting on stopped.
//
// A local model server is where this actually bites: it accepts the
// request and can then stop producing without closing the stream, with no
// server-side timeout to end it. Cloud providers close idle streams
// themselves, which is why nothing here was needed until a local endpoint
// was in the picture.
//
// The two budgets below are separate because the two silences mean
// different things:
//
//   - providerStreamFirstPartTimeout covers Stream call -> first part.
//     Nothing has been produced yet, and for a local model this window
//     legitimately includes prompt ingestion, which on a long context is
//     minutes of real work with nothing to show for it. Generous on
//     purpose.
//   - providerStreamStallTimeout covers the gap between two parts. Once a
//     provider is emitting, it emits steadily; a multi-minute hole after
//     the stream has started is a wedge, not throughput.
//
// Both are deliberately far above any healthy value rather than tuned
// close to one. This is a backstop against a stream that will never end,
// not a latency budget: it must never be what fails a slow-but-working
// request. They are hard constants for the same reason the delegation
// width caps are — a value someone could configure upward until it stops
// firing would defeat the reason it exists.
const (
	providerStreamFirstPartTimeout = 10 * time.Minute
	providerStreamStallTimeout     = 5 * time.Minute
)

// Stall phases, as reported on the error and in the finished log line.
const (
	stallPhaseFirstPart = "first_part"
	stallPhaseStream    = "stream"
)

// providerStallError is what a tripped watchdog surfaces in place of the
// silence. It implements net.Error with Timeout() true, which is what
// makes it retryable: fantasy's isRetryableError (third_party/fantasy,
// retry.go) treats any net.Error as transient, so a stalled attempt is
// re-attempted under the ordinary retry budget instead of failing the
// turn on the first hole. Only when every attempt stalls does the turn
// fail — and a failed turn is a terminal event, which is the whole point:
// the delegation finalizes and its parent is told, rather than both
// waiting forever.
//
// It deliberately does not wrap context.Canceled even though a
// cancellation is the mechanism that unblocks the read. fantasy's
// isAbortError treats context.Canceled as "the caller asked to stop" and
// refuses to retry it, and a stall is the opposite of that: nobody asked
// for anything, which is the problem.
type providerStallError struct {
	// phase is stallPhaseFirstPart or stallPhaseStream — which of the two
	// budgets ran out.
	phase string
	// limit is the budget that was exceeded, carried so the message says
	// what was actually waited for rather than a bare "timeout".
	limit time.Duration
}

func (e *providerStallError) Error() string {
	if e.phase == stallPhaseFirstPart {
		return fmt.Sprintf("provider stream produced nothing within %s", e.limit)
	}
	return fmt.Sprintf("provider stream stalled: no data for %s", e.limit)
}

// Timeout reports true so the error satisfies net.Error's retryable shape.
func (e *providerStallError) Timeout() bool { return true }

// Temporary is part of net.Error. It is deprecated in the standard
// library but still in the interface, and the answer is genuinely true: a
// stalled stream says nothing about whether the next attempt will stall.
func (e *providerStallError) Temporary() bool { return true }

// streamStallWatchdog cancels a stream's context once it has been silent
// for longer than the budget in force, and records why, so the caller can
// report the stall rather than the cancellation it caused.
//
// It owns a derived context rather than the caller's own: cancelling the
// caller's would end the whole turn, when what needs to end is this one
// attempt. Every method is safe to call from any goroutine — the timer
// fires on its own — and safe to call after stop, which is what lets the
// consumer beat on every part without checking whether the stream has
// already finished.
type streamStallWatchdog struct {
	cancel context.CancelFunc

	mu    sync.Mutex
	timer *time.Timer
	// phase and limit are the budget currently in force — first-part
	// until the stream produces something, between-parts afterwards.
	// They are the fields a trip reports, so they must always describe
	// the budget the armed timer is counting down.
	phase string
	limit time.Duration
	// gap is the between-parts budget, stashed at construction so the
	// consumer's per-part call stays a bare beat() with nothing to get
	// wrong at the call site.
	gap     time.Duration
	tripped *providerStallError
	stopped bool
}

// newStreamStallWatchdog derives a cancellable context from ctx and arms
// the first-part budget on it. The returned context is what the stream
// must be created with; the watchdog is what the consumer beats and then
// stops.
func newStreamStallWatchdog(ctx context.Context, firstPart, gap time.Duration) (context.Context, *streamStallWatchdog) {
	streamCtx, cancel := context.WithCancel(ctx)
	w := &streamStallWatchdog{
		cancel: cancel,
		phase:  stallPhaseFirstPart,
		limit:  firstPart,
		gap:    gap,
	}
	// Armed last, so the callback cannot observe a half-built watchdog:
	// AfterFunc can fire on its own goroutine the instant it is created.
	w.timer = time.AfterFunc(firstPart, w.fire)
	return streamCtx, w
}

// fire is the timer's callback: record the stall (once) and cancel the
// stream's context, which is what unblocks whatever read was hanging.
func (w *streamStallWatchdog) fire() {
	w.mu.Lock()
	if w.stopped || w.tripped != nil {
		w.mu.Unlock()
		return
	}
	w.tripped = &providerStallError{phase: w.phase, limit: w.limit}
	w.mu.Unlock()
	w.cancel()
}

// beat records that the stream produced something and re-arms the
// between-parts budget. A no-op once the watchdog has tripped or stopped:
// a tripped stream may still yield the parts that were already buffered
// before the cancellation landed, and those must not un-trip it.
func (w *streamStallWatchdog) beat() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || w.tripped != nil {
		return
	}
	w.phase, w.limit = stallPhaseStream, w.gap
	w.timer.Reset(w.gap)
}

// stop disarms the watchdog and releases the derived context. Idempotent.
// Cancelling here is safe and necessary: it runs only once the attempt is
// over (the stream errored at creation, or ranging has finished), so
// there is nothing left to interrupt, and skipping it would leak the
// context until the parent's own cancellation.
func (w *streamStallWatchdog) stop() {
	w.mu.Lock()
	already := w.stopped
	w.stopped = true
	timer := w.timer
	w.mu.Unlock()
	if already {
		return
	}
	timer.Stop()
	w.cancel()
}

// isProviderStall reports whether err is (or wraps) a stall recorded by a
// stream watchdog. Callers use it instead of a bare type assertion so the
// stall stays recognizable once fantasy's retry machinery has wrapped it
// in a RetryError.
func isProviderStall(err error) bool {
	_, ok := errors.AsType[*providerStallError](err)
	return ok
}

// stall returns the recorded stall, or nil if the watchdog never tripped.
func (w *streamStallWatchdog) stall() *providerStallError {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tripped
}
