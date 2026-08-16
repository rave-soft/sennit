package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// TestClose_SharedRegistryBrokerSurvivesOneOfTwoWorkspacesClosing is the
// regression test for ARCHITECTURE_REVIEW.md section 3.1: before the
// liveWorkspaces refcount, mcp.Close unconditionally called broker.Shutdown,
// so a second workspace calling app.New (which arms the process-global MCP
// registry and defers mcp.Close in its cleanup) would have its own MCP event
// stream killed the instant the FIRST workspace shut down, since both shared
// defaultRegistry.
//
// This drives the fix through the actual package-level API (ArmInit/Close),
// the same one app.New uses, against a scratch registry swapped in for
// defaultRegistry so the test can't leave the package's real shared registry
// permanently shut down for tests that run after it.
func TestClose_SharedRegistryBrokerSurvivesOneOfTwoWorkspacesClosing(t *testing.T) {
	orig := defaultRegistry
	defaultRegistry = NewRegistry()
	t.Cleanup(func() { defaultRegistry = orig })

	ctx := context.Background()

	// Two workspaces both arm against the same shared registry, exactly as
	// two concurrent app.New calls would.
	ArmInit()
	ArmInit()

	sub := SubscribeEvents(ctx)

	// The first workspace shuts down. The second is still alive, so the
	// shared broker (and this subscriber's channel) must survive.
	require.NoError(t, Close(ctx))

	defaultRegistry.broker.Publish(pubsub.UpdatedEvent, Event{
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
	require.NoError(t, Close(ctx))

	select {
	case _, ok := <-sub:
		require.False(t, ok, "subscriber channel should be closed once every workspace has closed")
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber channel was never closed after the last workspace closed")
	}
}

// TestRegistry_CloseIsolatedPerInstance is the stage-2 counterpart: two
// independent *Registry instances (simulating two workspaces that have each
// been given their own registry, per ARCHITECTURE_REVIEW.md 3.1's suggested
// fix) must not affect each other's broker at all — closing one must never
// touch the other's event stream, refcounted or not.
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
