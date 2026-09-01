package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// TestClose_SharedRegistryBrokerSurvivesOneOfTwoWorkspacesClosing verifies
// that a registry shared by two owners keeps its event broker alive until the
// last owner closes it.
func TestClose_SharedRegistryBrokerSurvivesOneOfTwoWorkspacesClosing(t *testing.T) {
	r := NewRegistry()

	ctx := context.Background()

	// Two workspaces both arm against the same shared registry, exactly as
	// two concurrent app.New calls would.
	r.ArmInit()
	r.ArmInit()

	sub := r.SubscribeEvents(ctx)

	// The first workspace shuts down. The second is still alive, so the
	// shared broker (and this subscriber's channel) must survive.
	require.NoError(t, r.Close(ctx))

	r.broker.Publish(pubsub.UpdatedEvent, Event{
		Type: EventStateChanged,
		Name: "still-alive",
	})
	select {
	case ev, ok := <-sub:
		require.True(t, ok, "subscriber channel was closed after only one of two workspaces closed")
		require.Equal(t, "still-alive", ev.Payload.Name)
	case <-time.After(2 * time.Second):
		t.Fatal("event was not delivered: broker appears to have shut down early")
	}

	// The second (last) workspace closes. Now the broker should shut down,
	// and the subscriber's channel should be closed.
	require.NoError(t, r.Close(ctx))

	select {
	case _, ok := <-sub:
		require.False(t, ok, "subscriber channel should be closed once every workspace has closed")
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber channel was never closed after the last workspace closed")
	}
}

// TestRegistry_CloseIsolatedPerInstance is the stage-2 counterpart: two
// independent *Registry instances (simulating two workspaces that have each
// been given their own registry) must not affect each other's broker at
// all — closing one must never touch the other's event stream,
// refcounted or not.
func TestRegistry_CloseContinuesAfterCallerCancellation(t *testing.T) {
	r := NewRegistry()
	r.ArmInit()
	waiter := &tokenWrite{done: make(chan struct{})}
	r.publishMu.Lock()
	r.tokenWrites[tokenWriteOwner{name: "blocked", attempt: attemptID{gen: 1, seq: 1}}] = map[*tokenWrite]struct{}{waiter: {}}
	r.publishMu.Unlock()
	sub := r.SubscribeEvents(context.Background())

	firstCtx, firstCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer firstCancel()
	require.ErrorIs(t, r.Close(firstCtx), context.DeadlineExceeded)

	secondDone := make(chan error, 1)
	go func() { secondDone <- r.Close(context.Background()) }()
	select {
	case err := <-secondDone:
		t.Fatalf("repeated Close returned before shared cleanup completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case _, ok := <-sub:
		if !ok {
			t.Fatal("broker shut down before shared cleanup completed")
		}
	default:
	}

	close(waiter.done)
	require.NoError(t, <-secondDone)
	select {
	case _, ok := <-sub:
		require.False(t, ok)
	case <-time.After(2 * time.Second):
		t.Fatal("broker did not shut down after shared cleanup completed")
	}
	require.NoError(t, r.Close(context.Background()))
}

func TestLifecycleCleanupContextPreservesValuesAfterCancellation(t *testing.T) {
	type contextKey struct{}

	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), contextKey{}, "cleanup-value"))
	cancel()
	cleanupCtx, cleanupCancel := lifecycleCleanupContext(ctx)
	defer cleanupCancel()

	require.NoError(t, cleanupCtx.Err())
	require.Equal(t, "cleanup-value", cleanupCtx.Value(contextKey{}))
	deadline, ok := cleanupCtx.Deadline()
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(lifecycleCleanupTimeout), deadline, 100*time.Millisecond)
}

func TestRegistry_CloseReturnsEachCallersContextError(t *testing.T) {
	r := NewRegistry()
	r.ArmInit()
	waiter := &tokenWrite{done: make(chan struct{})}
	r.publishMu.Lock()
	r.tokenWrites[tokenWriteOwner{name: "blocked", attempt: attemptID{gen: 1, seq: 1}}] = map[*tokenWrite]struct{}{waiter: {}}
	r.publishMu.Unlock()

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, r.Close(cancelledCtx), context.Canceled)
	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer deadlineCancel()
	require.ErrorIs(t, r.Close(deadlineCtx), context.DeadlineExceeded)
	close(waiter.done)
	require.NoError(t, r.Close(context.Background()))
}

func TestRegistry_CloseIsolatedPerInstance(t *testing.T) {
	regA := NewRegistry()
	regB := NewRegistry()

	ctx := context.Background()
	regA.ArmInit()
	regB.ArmInit()

	subA := regA.SubscribeEvents(ctx)
	subB := regB.SubscribeEvents(ctx)

	require.NoError(t, regA.Close(ctx))

	// regA's own broker is shut down (it was the only workspace on regA).
	select {
	case _, ok := <-subA:
		require.False(t, ok, "regA's subscriber channel should be closed after regA.Close")
	case <-time.After(2 * time.Second):
		t.Fatal("regA's subscriber channel was never closed")
	}

	// regB is a completely separate registry: it must be unaffected.
	regB.broker.Publish(pubsub.UpdatedEvent, Event{
		Type: EventStateChanged,
		Name: "regB-still-alive",
	})
	select {
	case ev, ok := <-subB:
		require.True(t, ok, "regB's subscriber channel must not be closed by regA.Close")
		require.Equal(t, "regB-still-alive", ev.Payload.Name)
	case <-time.After(2 * time.Second):
		t.Fatal("regB event was not delivered: closing regA must not affect regB's broker")
	}

	require.NoError(t, regB.Close(ctx))
}
