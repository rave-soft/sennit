package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/shell"
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

// TestApp_Shutdown_IsIdempotent verifies that repeated Shutdown calls are
// safe and only run teardown once.
func TestApp_Shutdown_IsIdempotent(t *testing.T) {
	t.Parallel()

	a := NewForTest(t.Context())
	require.NoError(t, a.AddShutdownHook(func(context.Context) error { return nil }))
	require.NoError(t, a.AddCleanup(func(context.Context) error { return nil }))

	a.Shutdown()
	a.Shutdown() // repeated
	a.Shutdown() // repeated

	require.Equal(t, shutdownStateDone, a.shutdownState)
}

// TestApp_Shutdown_Concurrent waits for multiple concurrent callers to
// return together after a single teardown.
func TestApp_Shutdown_Concurrent(t *testing.T) {
	t.Parallel()

	a := NewForTest(t.Context())
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { a.Shutdown() })
	}
	wg.Wait()

	require.Equal(t, shutdownStateDone, a.shutdownState)
}

func TestAppShutdownDoesNotAffectOtherBackgroundShells(t *testing.T) {
	t.Parallel()
	workingDir := t.TempDir()
	first, second := NewForTest(t.Context()), NewForTest(t.Context())
	first.BackgroundShells = shell.NewBackgroundShellManager()
	second.BackgroundShells = shell.NewBackgroundShellManager()
	firstJob, err := first.BackgroundShells.Start(t.Context(), workingDir, nil, "sleep 5", "")
	require.NoError(t, err)
	secondJob, err := second.BackgroundShells.Start(t.Context(), workingDir, nil, "sleep 5", "")
	require.NoError(t, err)
	first.Shutdown()
	require.True(t, firstJob.IsDone())
	require.False(t, secondJob.IsDone())
	second.Shutdown()
	require.True(t, secondJob.IsDone())
}

// TestApp_Shutdown_HooksRunBeforeCleanups verifies that shutdown hooks
// are called before cleanup functions.
func TestApp_Shutdown_HooksRunBeforeCleanups(t *testing.T) {
	t.Parallel()

	var order []string
	mu := sync.Mutex{}
	addOrder := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}

	a := NewForTest(t.Context())
	require.NoError(t, a.AddShutdownHook(func(context.Context) error {
		addOrder("hook-1")
		return nil
	}))
	require.NoError(t, a.AddCleanup(func(context.Context) error {
		addOrder("cleanup-1")
		return nil
	}))
	require.NoError(t, a.AddCleanup(func(context.Context) error {
		addOrder("cleanup-2")
		return nil
	}))

	a.Shutdown()

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"hook-1", "cleanup-1", "cleanup-2"}, order)
}

// TestApp_Shutdown_AddAfterStartRejects verifies that AddCleanup and
// AddShutdownHook return ErrAppShutdownBlocked after shutdown begins.
func TestApp_Shutdown_AddAfterStartRejects(t *testing.T) {
	t.Parallel()

	a := NewForTest(t.Context())

	// Register a hook that blocks so we can test mid-shutdown.
	barrier := make(chan struct{})
	require.NoError(t, a.AddShutdownHook(func(context.Context) error {
		<-barrier
		return nil
	}))

	// Start shutdown in background.
	done := make(chan struct{})
	go func() {
		a.Shutdown()
		close(done)
	}()

	// Wait for hook to block (shutdown in progress).
	time.Sleep(50 * time.Millisecond)

	// Now AddCleanup/AddShutdownHook should fail.
	require.ErrorIs(t, a.AddCleanup(func(context.Context) error { return nil }), ErrAppShutdownBlocked)
	require.ErrorIs(t, a.AddShutdownHook(func(context.Context) error { return nil }), ErrAppShutdownBlocked)

	// Release shutdown to let the test finish.
	close(barrier)
	<-done
}

// TestApp_Shutdown_BoundsBlockingCallbacks verifies shutdown and concurrent
// callers return when a cleanup ignores cancellation.
func TestApp_Shutdown_BoundsBlockingCallbacks(t *testing.T) {
	t.Parallel()

	a := NewForTest(t.Context())
	a.shutdownTimeout = 20 * time.Millisecond
	block := make(chan struct{})
	require.NoError(t, a.AddCleanup(func(context.Context) error {
		<-block
		return nil
	}))

	done := make(chan struct{})
	go func() {
		a.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not return after the cleanup deadline")
	}
	close(block)
}

// TestApp_Shutdown_SkipsCriticalCleanupAfterHookTimeout ensures a thread DB
// is not released while its manager hook is still running.
func TestApp_Shutdown_SkipsCriticalCleanupAfterHookTimeout(t *testing.T) {
	t.Parallel()

	a := NewForTest(t.Context())
	a.shutdownTimeout = 20 * time.Millisecond
	block := make(chan struct{})
	var released atomic.Bool
	require.NoError(t, a.AddShutdownHook(func(context.Context) error {
		<-block
		return nil
	}))
	require.NoError(t, a.AddCriticalCleanup(func(context.Context) error {
		released.Store(true)
		return nil
	}))

	a.Shutdown()
	require.False(t, released.Load())
	close(block)
}

// TestApp_Shutdown_ProductionResourcePhasesAreDependencySafe verifies the
// production-like registration sequence cannot make a generic cleanup close
// the main DB before watchers and MCP have stopped. Production resources use
// explicit phases rather than their position in cleanupFuncs.
func TestApp_Shutdown_ProductionResourcePhasesAreDependencySafe(t *testing.T) {
	t.Parallel()

	var order []string
	var mu sync.Mutex
	addOrder := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}

	a := NewForTest(t.Context())
	// Deliberately register a normal cleanup before the production resources,
	// matching the fact that their registration order is not a contract.
	require.NoError(t, a.AddCleanup(func(context.Context) error {
		addOrder("ordinary-cleanup")
		return nil
	}))
	require.NoError(t, a.AddPreCleanupHook(func(context.Context) error {
		addOrder("watchers-stopped")
		return nil
	}))
	a.mcpClose = func(context.Context) error {
		addOrder("mcp-closed")
		return nil
	}
	a.mainDBRelease = func(context.Context) error {
		addOrder("main-db-released")
		return nil
	}

	a.Shutdown()

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{
		"watchers-stopped",
		"mcp-closed",
		"ordinary-cleanup",
		"main-db-released",
	}, order)
}

// TestApp_Shutdown_PreCleanupHookFailureSkipsMCP ensures a watcher that has
// not stopped cannot race MCP shutdown; unrelated main DB release continues.
func TestApp_Shutdown_PreCleanupHookFailureRetainsDependencies(t *testing.T) {
	t.Parallel()

	a := NewForTest(t.Context())
	var mcpClosed, dbReleased atomic.Bool
	require.NoError(t, a.AddPreCleanupHook(func(context.Context) error {
		return fmt.Errorf("watcher did not stop")
	}))
	a.mcpClose = func(context.Context) error {
		mcpClosed.Store(true)
		return nil
	}
	a.mainDBRelease = func(context.Context) error {
		dbReleased.Store(true)
		return nil
	}

	a.Shutdown()
	require.False(t, mcpClosed.Load())
	require.False(t, dbReleased.Load())
}

func TestApp_Shutdown_MCPInitBeforeCloseAndDB(t *testing.T) {
	t.Parallel()

	a := NewForTest(t.Context())
	initStopped := make(chan struct{})
	a.mcpInitCancel = func() { close(initStopped) }
	a.mcpInitWG.Add(1)
	go func() {
		defer a.mcpInitWG.Done()
		<-initStopped
	}()

	var order []string
	a.mcpClose = func(context.Context) error {
		select {
		case <-initStopped:
		default:
			t.Fatal("MCP initialization was not stopped before close")
		}
		order = append(order, "mcp")
		return nil
	}
	a.mainDBRelease = func(context.Context) error {
		order = append(order, "db")
		return nil
	}

	a.Shutdown()
	require.Equal(t, []string{"mcp", "db"}, order)
}

func TestApp_Shutdown_ManagerBeforeDB(t *testing.T) {
	t.Parallel()

	var order []string
	mu := sync.Mutex{}
	addOrder := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}

	a := NewForTest(t.Context())
	// Simulate thread manager shutdown (runs as hook).
	require.NoError(t, a.AddShutdownHook(func(context.Context) error {
		addOrder("manager-shutdown")
		return nil
	}))
	// Simulate DB release (runs as cleanup).
	require.NoError(t, a.AddCriticalCleanup(func(context.Context) error {
		addOrder("db-release")
		return nil
	}))

	a.Shutdown()

	mu.Lock()
	defer mu.Unlock()
	// Manager shutdown must appear before DB release.
	for i, s := range order {
		if s == "db-release" {
			found := false
			for _, prev := range order[:i] {
				if prev == "manager-shutdown" {
					found = true
				}
			}
			require.True(t, found, "manager-shutdown must appear before db-release")
		}
	}
}

func TestApp_Shutdown_FinalCleanupRunsAfterRepositoryUsers(t *testing.T) {
	a := NewForTest(t.Context())
	var order []string
	var mu sync.Mutex
	add := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, name)
	}
	a.stopBackgroundShells = func(context.Context) error {
		add("shells")
		return nil
	}
	a.stopLSP = func(context.Context) error {
		add("lsp")
		return nil
	}
	require.NoError(t, a.AddFinalCleanup(func(context.Context) error {
		add("lock")
		return nil
	}))

	a.Shutdown()
	a.Shutdown() // Final cleanups, like the lock release, remain idempotent.

	mu.Lock()
	defer mu.Unlock()
	require.ElementsMatch(t, []string{"shells", "lsp", "lock"}, order)
	require.Equal(t, "lock", order[len(order)-1])
}

func TestApp_Shutdown_FinalCleanupRetainedAfterShutdownHookFailure(t *testing.T) {
	a := NewForTest(t.Context())
	var released atomic.Bool
	require.NoError(t, a.AddShutdownHook(func(context.Context) error {
		return errors.New("thread manager did not stop")
	}))
	require.NoError(t, a.AddFinalCleanup(func(context.Context) error {
		released.Store(true)
		return nil
	}))

	a.Shutdown()
	require.False(t, released.Load())
}

func TestApp_Shutdown_FinalCleanupRetainedAfterShutdownHookTimeout(t *testing.T) {
	a := NewForTest(t.Context())
	a.shutdownTimeout = 20 * time.Millisecond
	block := make(chan struct{})
	var released atomic.Bool
	require.NoError(t, a.AddShutdownHook(func(context.Context) error {
		<-block
		return nil
	}))
	require.NoError(t, a.AddFinalCleanup(func(context.Context) error {
		released.Store(true)
		return nil
	}))

	a.Shutdown()
	require.False(t, released.Load())
	close(block)
}

func TestApp_Shutdown_FinalCleanupRetainedWhileAgentWorkRemains(t *testing.T) {
	a := NewForTest(t.Context())
	a.agentWorkStopped = func() bool { return false }
	var released atomic.Bool
	require.NoError(t, a.AddFinalCleanup(func(context.Context) error {
		released.Store(true)
		return nil
	}))

	a.Shutdown()
	require.False(t, released.Load())
}

func TestApp_Shutdown_FinalCleanupRetainedAfterRepositoryUserTimeout(t *testing.T) {
	a := NewForTest(t.Context())
	a.shutdownTimeout = 20 * time.Millisecond
	block := make(chan struct{})
	var released atomic.Bool
	a.stopBackgroundShells = func(context.Context) error {
		<-block
		return nil
	}
	require.NoError(t, a.AddFinalCleanup(func(context.Context) error {
		released.Store(true)
		return nil
	}))

	a.Shutdown()
	require.False(t, released.Load())
	close(block)
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
