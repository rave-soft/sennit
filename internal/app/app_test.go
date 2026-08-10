package app

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// TestSetupSubscriber_NormalFlow verifies that events published to the source
// broker are forwarded to the output broker.
func TestSetupSubscriber_NormalFlow(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	src := pubsub.NewBroker[string]()
	defer src.Shutdown()
	out := pubsub.NewBroker[any]()
	defer out.Shutdown()

	ch := out.Subscribe(ctx)

	var wg sync.WaitGroup
	setupSubscriber(ctx, &wg, "test", src.Subscribe, out)

	// Yield so the subscriber goroutine can call src.Subscribe before we publish.
	time.Sleep(10 * time.Millisecond)

	src.Publish(pubsub.CreatedEvent, "hello")
	src.Publish(pubsub.CreatedEvent, "world")

	for range 2 {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for forwarded event")
		}
	}

	cancel()
	wg.Wait()
}

// TestSetupSubscriber_ContextCancellation verifies the goroutine exits cleanly
// when the context is cancelled.
func TestSetupSubscriber_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	src := pubsub.NewBroker[string]()
	defer src.Shutdown()
	out := pubsub.NewBroker[any]()
	defer out.Shutdown()

	var wg sync.WaitGroup
	setupSubscriber(ctx, &wg, "test", src.Subscribe, out)

	src.Publish(pubsub.CreatedEvent, "event")
	cancel()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("setupSubscriber goroutine did not exit after context cancellation")
	}
}

// TestEvents_ZeroConsumers verifies that publishing with no subscribers does
// not block or panic.
func TestEvents_ZeroConsumers(t *testing.T) {
	t.Parallel()

	broker := pubsub.NewBroker[any]()
	defer broker.Shutdown()

	require.Equal(t, 0, broker.GetSubscriberCount())

	// Must not block.
	done := make(chan struct{})
	go func() {
		broker.Publish(pubsub.UpdatedEvent, any("msg1"))
		broker.Publish(pubsub.UpdatedEvent, any("msg2"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish with zero consumers blocked")
	}
}

// TestEvents_OneConsumer verifies that a single subscriber receives every event
// exactly once.
func TestEvents_OneConsumer(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	broker := pubsub.NewBroker[any]()
	defer broker.Shutdown()

	ch := broker.Subscribe(ctx)

	const n = 10
	for i := range n {
		broker.Publish(pubsub.UpdatedEvent, any(i))
	}

	for i := range n {
		select {
		case ev := <-ch:
			require.Equal(t, any(i), ev.Payload)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for event %d", i)
		}
	}
}

// TestEvents_NConsumers verifies that every subscriber receives every event
// exactly once, regardless of how many concurrent consumers are attached.
func TestEvents_NConsumers(t *testing.T) {
	t.Parallel()

	for _, n := range []int{2, 5, 10} {
		t.Run(fmt.Sprintf("consumers=%d", n), func(t *testing.T) {
			t.Parallel()
			testNConsumers(t, n)
		})
	}
}

// TestForwardEvents verifies that ForwardEvents subscribes to a
// post-construction event source (standing in for a thread manager
// attached after New(), see internal/cmd/threads.go) and fans its events
// into app.events, so they surface through App.Events like every
// statically-wired source.
func TestForwardEvents(t *testing.T) {
	t.Parallel()

	type fakeThreadEvent struct{ ID string }

	a := NewForTest(t.Context())
	defer a.ShutdownForTest()

	src := pubsub.NewBroker[fakeThreadEvent]()
	defer src.Shutdown()

	ch := a.Events(t.Context())

	ForwardEvents(a, "test-thread", src.Subscribe)

	// Yield so the forwarding goroutine can call src.Subscribe before we
	// publish; otherwise the publish could race ahead of the subscribe.
	time.Sleep(10 * time.Millisecond)

	src.Publish(pubsub.UpdatedEvent, fakeThreadEvent{ID: "thread-1"})

	select {
	case ev := <-ch:
		payload, ok := ev.Payload.(pubsub.Event[fakeThreadEvent])
		require.True(t, ok, "unexpected payload type %T", ev.Payload)
		require.Equal(t, "thread-1", payload.Payload.ID)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for forwarded event")
	}
}

func testNConsumers(t *testing.T, n int) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	broker := pubsub.NewBroker[any]()
	defer broker.Shutdown()

	// Subscribe all N consumers before publishing.
	channels := make([]<-chan pubsub.Event[any], n)
	for i := range n {
		channels[i] = broker.Subscribe(ctx)
	}
	require.Equal(t, n, broker.GetSubscriberCount())

	const numEvents = 20
	for i := range numEvents {
		broker.Publish(pubsub.UpdatedEvent, any(i))
	}

	// Each consumer must receive all numEvents messages.
	var wg sync.WaitGroup
	for i, ch := range channels {
		wg.Go(func() {
			for j := range numEvents {
				select {
				case ev := <-ch:
					require.Equal(t, any(j), ev.Payload,
						"consumer %d: wrong payload for event %d", i, j)
				case <-time.After(5 * time.Second):
					t.Errorf("consumer %d: timed out waiting for event %d", i, j)
					return
				}
			}
		})
	}
	wg.Wait()
}
