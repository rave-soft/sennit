package model

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/util"
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

// TestRootDeliversEnvelopedResultToTheUIThatDispatchedIt is the ownedMsg
// counterpart of TestRootDeliversOwnedResultsToTheUIThatAskedForThem:
// util.ClearStatusMsg is defined outside model (in internal/ui/util), so it
// cannot embed uiOwned directly — it is tagged via ownCmd's envelope
// instead (see root.go). Proves that path also survives a screen switch
// between dispatch and arrival, not just the twelve types that embed
// uiOwned themselves.
func TestRootDeliversEnvelopedResultToTheUIThatDispatchedIt(t *testing.T) {
	ws := &countingWorkspace{ready: true}
	r := &Root{com: newBusyUI(ws).com, main: newBusyUI(ws), active: screenMain}

	cmd := ownCmd(r.main, r.main.status.ShowInfo(util.InfoMsg{Msg: "saved", TTL: time.Millisecond}))
	require.NotNil(t, cmd)
	require.False(t, r.main.status.InfoMsg().IsEmpty())

	// The user opens the threads dashboard before the clear timer fires.
	r.active = screenDashboard

	msg := cmd()
	require.IsType(t, ownedMsg{}, msg, "a cross-package type must arrive wrapped in the envelope")
	r.Update(msg)

	require.True(t, r.main.status.InfoMsg().IsEmpty(),
		"the enveloped result must reach the UI that dispatched it, whatever screen is on top")
}

// TestRootRoutesEnvelopedResultAwayFromTheOtherUI is the ownedMsg
// counterpart of TestRootRoutesAnOwnedResultAwayFromTheOtherUI: both UIs
// dispatch the exact same wrapped message type (util.ClearStatusMsg inside
// ownedMsg), so only the owner pointer captured at dispatch time — not the
// type, and not a thread-ID comparison the way threadEventMsg needs — can
// be what keeps them apart.
func TestRootRoutesEnvelopedResultAwayFromTheOtherUI(t *testing.T) {
	ws := &countingWorkspace{ready: true}
	r := &Root{com: newBusyUI(ws).com, main: newBusyUI(ws), active: screenThread}
	threadUI := newBusyUI(ws)
	r.attachment.thread = &threadAttachment{threadID: "t1", ui: threadUI}

	mainCmd := ownCmd(r.main, r.main.status.ShowInfo(util.InfoMsg{Msg: "main", TTL: time.Millisecond}))
	threadCmd := ownCmd(threadUI, threadUI.status.ShowInfo(util.InfoMsg{Msg: "thread", TTL: time.Millisecond}))
	require.NotNil(t, mainCmd)
	require.NotNil(t, threadCmd)

	// The thread's own timer fires first; the main screen's is still
	// pending.
	r.Update(threadCmd())

	require.True(t, threadUI.status.InfoMsg().IsEmpty(), "the thread's own result must reach it")
	require.False(t, r.main.status.InfoMsg().IsEmpty(),
		"the thread's result must not clear the main screen's unrelated status message")

	// The main screen's own timer, delivered afterward, still finds it.
	r.Update(mainCmd())
	require.True(t, r.main.status.InfoMsg().IsEmpty(), "the main screen's own result must reach it too")
}

// TestRootDeliversYoloToggleResultAwayFromTheOtherUI is a second-pass
// (directly embedded, not enveloped) type from the same routing fix as
// TestRootRoutesEnvelopedResultAwayFromTheOtherUI: yoloToggledMsg is
// defined in model, so it embeds uiOwned itself rather than going through
// ownCmd's envelope. Proves the plain-embedding path also keeps two UIs'
// in-flight operations apart when both dispatch the same message type at
// once.
func TestRootDeliversYoloToggleResultAwayFromTheOtherUI(t *testing.T) {
	ws := &countingWorkspace{ready: true}
	r := &Root{com: newBusyUI(ws).com, main: newBusyUI(ws), active: screenThread}
	threadUI := newBusyUI(ws)
	r.attachment.thread = &threadAttachment{threadID: "t1", ui: threadUI}

	mainCmd := r.main.toggleYoloMode()
	threadCmd := threadUI.toggleYoloMode()
	require.NotNil(t, mainCmd)
	require.NotNil(t, threadCmd)
	require.True(t, r.main.yolo.loading)
	require.True(t, threadUI.yolo.loading)

	// The thread's own toggle completes first; the main screen's is still
	// in flight.
	r.Update(threadCmd())

	require.False(t, threadUI.yolo.loading, "the thread's own result must reach it")
	require.True(t, r.main.yolo.loading,
		"the thread's result must not clear the main screen's unrelated in-flight toggle")

	r.Update(mainCmd())
	require.False(t, r.main.yolo.loading, "the main screen's own result must reach it too")
}
