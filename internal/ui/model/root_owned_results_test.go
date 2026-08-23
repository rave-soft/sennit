package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRootDeliversOwnedResultsToTheUIThatAskedForThem pins the routing
// that keeps a screen switch from wedging a refresh for good. The main
// screen's probes are dispatched with an in-flight flag set and cleared
// only by their own result; routed by active screen, a result landing
// while the dashboard was up reached handleDashboardMsg — which forwards
// nothing but mouse events — and the flag stayed set forever, so that
// refresh never ran again for the rest of the session.
func TestRootDeliversOwnedResultsToTheUIThatAskedForThem(t *testing.T) {
	ws := &countingWorkspace{ready: true, agentBusy: true}
	r := &Root{com: newBusyUI(ws).com, main: newBusyUI(ws), active: screenMain}

	// A probe goes out from the main screen…
	cmd := r.main.dispatchBusyRefresh()
	require.NotNil(t, cmd, "the main screen must have started a probe")
	require.True(t, r.main.wsCache.busyFetchInFlight)

	// …the user opens the threads dashboard before it lands…
	r.active = screenDashboard

	// …and the result arrives.
	msg := cmd()
	require.IsType(t, busyStateMsg{}, msg)
	r.Update(msg)

	require.False(t, r.main.wsCache.busyFetchInFlight,
		"the result must reach the UI that dispatched it, whatever screen is on top")
}

// TestRootRoutesAnOwnedResultAwayFromTheOtherUI is the other half: a
// thread's own probe must not be applied to the main screen's state, and
// vice versa. Both UIs use the same message types, so the owner — not the
// type — is what decides.
func TestRootRoutesAnOwnedResultAwayFromTheOtherUI(t *testing.T) {
	ws := &countingWorkspace{ready: true, agentBusy: true}
	r := &Root{com: newBusyUI(ws).com, main: newBusyUI(ws), active: screenMain}
	threadUI := newBusyUI(ws)
	r.thread = &threadAttachment{threadID: "t1", ui: threadUI}
	r.active = screenThread

	cmd := threadUI.dispatchBusyRefresh()
	require.NotNil(t, cmd)
	require.True(t, threadUI.wsCache.busyFetchInFlight)

	// The main screen has a probe of its own in flight at the same time.
	require.NotNil(t, r.main.dispatchBusyRefresh())
	require.True(t, r.main.wsCache.busyFetchInFlight)

	r.Update(cmd())

	require.False(t, threadUI.wsCache.busyFetchInFlight, "the thread's own result must reach it")
	require.True(t, r.main.wsCache.busyFetchInFlight,
		"the thread's result must not clear the main screen's unrelated probe")
}
