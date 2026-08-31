package model

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/proto"
)

// taskSupportingWorkspace overrides countingWorkspace's hardcoded
// SupportsTasks/ListTasks (false/panic) so a test can dispatch a real
// agentListCache refresh without pulling in threadsTestWorkspace's
// unimplemented Config, which DefaultCommon needs.
type taskSupportingWorkspace struct {
	*countingWorkspace
}

func (w *taskSupportingWorkspace) SupportsTasks() bool { return true }

func (w *taskSupportingWorkspace) ListTasks(context.Context) ([]proto.Thread, error) {
	return nil, nil
}

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

// TestRootDeliversAgentsLoadedToTheUIThatAskedForIt is the agents-cache
// counterpart of TestRootDeliversOwnedResultsToTheUIThatAskedForThem: the
// session panel's delegation list re-probes every agentsCacheTTL while a
// delegation runs, and used to route by active screen like every other
// result here. Land one while the dashboard is on top and
// handleDashboardMsg drops it (it forwards nothing but mouse/wheel), which
// leaves agentListCache.cache.InFlight stuck true — DispatchRefresh then
// returns nil forever, and the agents section blocks for the rest of the
// session.
func TestRootDeliversAgentsLoadedToTheUIThatAskedForIt(t *testing.T) {
	ws := &taskSupportingWorkspace{countingWorkspace: &countingWorkspace{ready: true}}
	r := &Root{com: newBusyUI(ws).com, main: newBusyUI(ws), active: screenMain}

	cmd := r.main.agentList.staleRefreshCmd(r.main.com, r.main, true)
	require.NotNil(t, cmd, "the main screen must have started a delegation-list probe")
	require.True(t, r.main.agentList.cache.InFlight)

	r.active = screenDashboard

	msg := cmd()
	require.IsType(t, agentsLoadedMsg{}, msg)
	r.Update(msg)

	require.False(t, r.main.agentList.cache.InFlight,
		"the result must reach the UI that dispatched it, whatever screen is on top")
}

// TestRootDeliversShellResultToTheUIThatAskedForIt is the bang-command
// counterpart: a running `!` shell command's completion used to route by
// active screen too. Land it while the dashboard is on top and it is
// dropped, leaving pendingSendActive stuck true — every later sendMessage
// then queues behind it forever, since nothing else ever drains the queue.
func TestRootDeliversShellResultToTheUIThatAskedForIt(t *testing.T) {
	ws := &countingWorkspace{ready: true}
	r := &Root{com: newBusyUI(ws).com, main: newBusyUI(ws), active: screenMain}
	r.main.editor.pendingSend.active = true

	r.active = screenDashboard

	msg := shellResultMsg{
		uiOwned:    uiOwned{owner: r.main},
		PendingID:  "p1",
		Command:    "go test ./...",
		Output:     "ok",
		sessionID:  r.main.sess.current.ID,
		generation: r.main.sess.loadGen,
	}
	r.Update(msg)

	require.False(t, r.main.editor.pendingSend.active,
		"the shell result must reach the UI that dispatched it, whatever screen is on top")
}

// TestRootRoutesAnOwnedResultAwayFromTheOtherUI is the other half: a
// thread's own probe must not be applied to the main screen's state, and
// vice versa. Both UIs use the same message types, so the owner — not the
// type — is what decides.
func TestRootRoutesAnOwnedResultAwayFromTheOtherUI(t *testing.T) {
	ws := &countingWorkspace{ready: true, agentBusy: true}
	r := &Root{com: newBusyUI(ws).com, main: newBusyUI(ws), active: screenMain}
	threadUI := newBusyUI(ws)
	r.attachment.thread = &threadAttachment{threadID: "t1", ui: threadUI}
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
