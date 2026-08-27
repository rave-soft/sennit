package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestApp_AgentDispatcher_NonNilConfiguredOrNot asserts that an App
// built by New/NewForTest always hands back a usable dispatcher, even
// before any coordinator exists — the unconfigured-project case New
// handles by returning early. NewForTest never installs a coordinator,
// which is a stand-in for exactly that case.
func TestApp_AgentDispatcher_NonNilConfiguredOrNot(t *testing.T) {
	t.Parallel()

	a := NewForTest(t.Context())
	require.NotNil(t, a.AgentDispatcher())
	require.Nil(t, a.Coordinator())

	err := a.AgentDispatcher().Send("S1", "", "hi", nil)
	require.ErrorIs(t, err, ErrCoordinatorNotInitialized)
}

// TestApp_AgentDispatcher_SendRefusedAfterMarkClosing asserts Send on
// the App's own dispatcher (not a hand-rolled one) is refused once the
// gate is closed, mirroring TestAgentDispatcher_SendRefusedAfterMarkClosing
// but through the App-level accessor a later step (internal/workspace)
// will actually call.
func TestApp_AgentDispatcher_SendRefusedAfterMarkClosing(t *testing.T) {
	t.Parallel()

	a := NewForTest(t.Context())
	a.SetAgentCoordinatorForTest(&stubDispatchCoordinator{})

	a.AgentDispatcher().MarkClosing()

	err := a.AgentDispatcher().Send("S1", "run-1", "hi", nil)
	require.ErrorIs(t, err, ErrDispatcherClosed)
}

// TestApp_Shutdown_AgentDispatcherJoinedBeforeDBAndMCP proves the
// ordering promised by Shutdown's doc comment: a run dispatched through
// the App's own AgentDispatcher is joined before MCP is closed and the
// main DB is released, not merely "eventually" — Shutdown must not
// even return while the run is still in flight.
//
// The fake coordinator's RunAccepted blocks on a channel the test
// controls, so completion order is observed, not assumed from timing:
// Shutdown is asserted to NOT have finished (bounded wait, not a
// sleep-and-hope) before the run is released, and the mcp-closed/
// main-db-released cleanups are asserted to run only after Shutdown
// returns. A sleep-based version of this test would pass whether or not
// the Wait call in Shutdown's agent-work phase actually exists; this
// one cannot, because nothing unblocks Shutdown until the test releases
// the run.
func TestApp_Shutdown_AgentDispatcherJoinedBeforeDBAndMCP(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var order []string
	addOrder := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}

	a := NewForTest(t.Context())
	entered := make(chan struct{})
	release := make(chan struct{})
	coord := &stubDispatchCoordinator{entered: entered, release: release}
	a.SetAgentCoordinatorForTest(coord)

	a.mcpClose = func(context.Context) error {
		addOrder("mcp-closed")
		return nil
	}
	a.mainDBRelease = func(context.Context) error {
		addOrder("main-db-released")
		return nil
	}

	require.NoError(t, a.AgentDispatcher().Send("S1", "run-1", "hi", nil))

	// Wait for the run to actually be inside RunAccepted before starting
	// shutdown, so Shutdown genuinely races a run that is in flight
	// rather than one that has not been scheduled yet.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatched run never entered RunAccepted")
	}

	shutdownDone := make(chan struct{})
	go func() {
		a.Shutdown()
		close(shutdownDone)
	}()

	select {
	case <-shutdownDone:
		t.Fatal("Shutdown completed while a dispatched run was still blocked in RunAccepted")
	case <-time.After(150 * time.Millisecond):
	}

	mu.Lock()
	require.Empty(t, order, "mcp/DB cleanup ran before the in-flight run was joined")
	mu.Unlock()

	close(release)

	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not complete after the blocked run was released")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"mcp-closed", "main-db-released"}, order)
	require.Equal(t, int32(1), coord.runCount.Load())
}
