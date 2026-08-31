package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// swapInitGate replaces a registry's initDone channel with a fresh, open one
// for the duration of the test. This lets each test drive WaitForInit
// deterministically without shared process state.
func swapInitGate(t *testing.T, r *Registry) chan struct{} {
	t.Helper()
	orig := r.initDone
	r.initDone = make(chan struct{})

	r.initMu.Lock()
	origStarted := r.initStarted
	r.initStarted = true
	r.initMu.Unlock()

	t.Cleanup(func() {
		r.initDone = orig
		r.initMu.Lock()
		r.initStarted = origStarted
		r.initMu.Unlock()
	})
	return r.initDone
}

// TestWaitForInit_BlocksUntilInitCompletes pins the contract the coordinator
// relies on: WaitForInit blocks while MCP initialization is still in flight and
// returns once it completes. The coordinator calls it before reading the tool
// registry so slow-to-start servers (e.g. stdio Python via uv) have registered
// their tools first.
func TestWaitForInit_BlocksUntilInitCompletes(t *testing.T) {
	r := NewRegistry()
	gate := swapInitGate(t, r)

	// Init not done yet: WaitForInit must block until the context expires.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, r.WaitForInit(ctx), context.DeadlineExceeded,
		"WaitForInit must block while initialization is in flight")

	// Once initialization completes (the gate closes), WaitForInit returns nil.
	close(gate)
	require.NoError(t, r.WaitForInit(context.Background()),
		"WaitForInit must return once initialization has completed")
}

// TestWaitForInit_ReturnsWhenNotArmed is the regression test for coordinators
// built outside app startup. Those paths never call mcp.Initialize (which is
// what arms the gate), so WaitForInit must return immediately instead of
// blocking on a channel that will never close. Before the fix it blocked until
// ctx was cancelled, hanging coordinator.run's readyWg forever.
func TestWaitForInit_ReturnsWhenNotArmed(t *testing.T) {
	r := NewRegistry()
	// Ensure the gate looks unarmed regardless of test ordering.
	r.initMu.Lock()
	orig := r.initStarted
	r.initStarted = false
	r.initMu.Unlock()
	t.Cleanup(func() {
		r.initMu.Lock()
		r.initStarted = orig
		r.initMu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, r.WaitForInit(ctx),
		"WaitForInit must return immediately when initialization was never armed")
}

// TestWaitForInit_ToolsVisibleAfterInit is the regression test for the bug the
// coordinator fix addresses: buildTools read r.allTools concurrently with MCP
// initialization, so a slow server's tools were silently missing from the LLM's
// palette even though sennit_info later reported the server as connected. Gating
// on WaitForInit fixes it — any tool registered before initialization completes
// must be visible once WaitForInit returns.
func TestWaitForInit_ToolsVisibleAfterInit(t *testing.T) {
	r := NewRegistry()
	const name = "test-waitforinit-tools"
	t.Cleanup(func() {
		if s, ok := r.sessions.Take(name); ok {
			_ = s.Close()
		}
		r.allTools.Del(name)
		r.states.Del(name)
	})

	sess, _ := liveSession(t, "slow_tool")
	gate := swapInitGate(t, r)

	// A slow MCP server registers its tools, then initialization completes
	// (the gate closes). close(gate) happens-after the registration, and
	// WaitForInit returning happens-after observing the close, so the tools are
	// guaranteed visible once WaitForInit returns.
	go func() {
		r.sessions.Set(name, sess)
		r.allTools.Set(name, []*Tool{{Name: "slow_tool"}})
		r.updateState(name, StateConnected, nil, sess, Counts{Tools: 1})
		close(gate)
	}()

	require.NoError(t, r.WaitForInit(context.Background()))

	tools, ok := r.allTools.Get(name)
	require.True(t, ok, "a slow server's tools must be visible after WaitForInit returns")
	require.Len(t, tools, 1)
	require.Equal(t, "slow_tool", tools[0].Name)
}
