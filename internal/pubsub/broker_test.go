package pubsub

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestBroker_PublishMustDeliverDoesNotBlockSubscribe verifies that a slow
// subscriber being served by a blocking PublishMustDeliver call does not
// stall Subscribe for a concurrent caller. Before the broker moved to
// per-subscription locks, PublishMustDeliver held the broker-wide RWMutex
// for the entire per-subscriber timeout window, and Subscribe/unsubscribe
// need the same mutex for writing — so a single slow subscriber could
// freeze every other Subscribe call for up to the timeout.
func TestBroker_PublishMustDeliverDoesNotBlockSubscribe(t *testing.T) {
	t.Parallel()

	b := NewBrokerWithOptions[int](1)
	b.SetMustDeliverTimeout(200 * time.Millisecond)

	ctx := t.Context()
	slow := b.Subscribe(ctx)

	// Fill the slow subscriber's one-slot buffer so PublishMustDeliver's
	// fast path fails and it falls into the bounded blocking path.
	b.Publish(UpdatedEvent, 0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.PublishMustDeliver(ctx, UpdatedEvent, 1)
	}()

	// Give PublishMustDeliver time to enter its blocking send before we
	// race Subscribe against it.
	time.Sleep(20 * time.Millisecond)

	subscribeDone := make(chan struct{})
	go func() {
		defer close(subscribeDone)
		b.Subscribe(ctx)
	}()

	select {
	case <-subscribeDone:
		// Subscribe returned promptly; it was not blocked by the slow
		// PublishMustDeliver call.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Subscribe blocked on a concurrent slow PublishMustDeliver call")
	}

	// Drain the slow subscriber so PublishMustDeliver's blocking send
	// completes without waiting out the full timeout.
	<-slow

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("PublishMustDeliver did not return")
	}
}

// TestBroker_SubscriptionGoroutinesExitOnShutdown verifies that the
// per-subscription goroutine spawned by Subscribe terminates once
// Shutdown runs, even when the subscriber's own context is never
// canceled (e.g. a subscriber built with a background context, common
// for process-global brokers). Before b.done was added to that
// goroutine's select, Shutdown closed the channel directly and left the
// goroutine parked on <-ctx.Done() forever.
func TestBroker_SubscriptionGoroutinesExitOnShutdown(t *testing.T) {
	b := NewBroker[int]()

	const n = 50
	before := runtime.NumGoroutine()
	for range n {
		b.Subscribe(context.Background())
	}

	b.Shutdown()

	require.Eventually(t, func() bool {
		return runtime.NumGoroutine() <= before+1 // allow slack for scheduler noise
	}, time.Second, 10*time.Millisecond, "subscription goroutines leaked past Shutdown")
}

// TestBroker_DropCountsPreserveSemantics is a regression check that the
// refactor to per-subscription locking kept the counting behavior:
// Publish counts a drop only when the buffer is actually full, and
// PublishMustDeliver counts a drop only after its timeout elapses.
func TestBroker_DropCountsPreserveSemantics(t *testing.T) {
	t.Parallel()

	b := NewBrokerWithOptions[int](1)
	b.SetMustDeliverTimeout(20 * time.Millisecond)
	ctx := t.Context()

	sub := b.Subscribe(ctx)

	// First lossy publish fits in the buffer; no drop.
	b.Publish(UpdatedEvent, 1)
	require.Equal(t, uint64(0), b.DropCount())

	// Second lossy publish finds the buffer full; one drop.
	b.Publish(UpdatedEvent, 2)
	require.Equal(t, uint64(1), b.DropCount())

	<-sub // Drain so the next publish has room.

	// PublishMustDeliver with an empty buffer delivers immediately.
	b.PublishMustDeliver(ctx, UpdatedEvent, 3)
	require.Equal(t, uint64(0), b.MustDeliverDropCount())
	<-sub

	// Fill the buffer, then PublishMustDeliver must time out and count
	// exactly one must-deliver drop.
	b.Publish(UpdatedEvent, 4)
	b.PublishMustDeliver(ctx, UpdatedEvent, 5)
	require.Equal(t, uint64(1), b.MustDeliverDropCount())
}

// TestBroker_ConcurrentPublishAndSubscribeNoRace exercises concurrent
// Subscribe/unsubscribe alongside concurrent Publish/PublishMustDeliver
// to give the race detector a chance to catch any lock ordering or
// send-after-close bug introduced by moving to per-subscription locks.
// Run with -race.
func TestBroker_ConcurrentPublishAndSubscribeNoRace(t *testing.T) {
	t.Parallel()

	b := NewBrokerWithOptions[int](8)
	b.SetMustDeliverTimeout(5 * time.Millisecond)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	var published atomic.Int64
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				b.Publish(UpdatedEvent, int(published.Add(1)))
			}
		}
	}()
	go func() {
		defer wg.Done()
		ctx := context.Background()
		for {
			select {
			case <-stop:
				return
			default:
				b.PublishMustDeliver(ctx, UpdatedEvent, int(published.Add(1)))
			}
		}
	}()

	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				ctx, cancel := context.WithCancel(context.Background())
				ch := b.Subscribe(ctx)
				// Drain a little so publishers don't only ever see a
				// full buffer.
				select {
				case <-ch:
				default:
				}
				cancel()
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
	b.Shutdown()
}
