package model

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/threads"
)

// TestDashboardForwardsPermissionRequestToMain is the regression test for
// the agent hang described in handleDashboardMsg's doc comment: a
// permission request published while the threads dashboard is open used to
// be dropped by handleDashboardMsg's final "anything else is dropped"
// branch, so its dialog never opened and permission.Service — which does
// not re-send — blocked forever waiting on an answer nothing could give.
// The dashboard now forwards it to r.main, the only *UI alive while the
// dashboard is up.
func TestDashboardForwardsPermissionRequestToMain(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	model, _ := r.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	r = model.(*Root)
	r.dashboard = threads.New(r.com, &r.main.threadList)
	r.dashboard.SetSize(120, 40)
	r.active = screenDashboard

	perm := permission.PermissionRequest{ID: "perm-1", ToolCallID: "tc-1", ToolName: "bash"}
	model, _ = r.Update(pubsub.Event[permission.PermissionRequest]{Payload: perm})
	r = model.(*Root)

	require.Equal(t, screenDashboard, r.active, "the dashboard must stay on top")
	require.True(t, r.main.dialog.ContainsDialog(dialog.PermissionsID),
		"a permission request published while the dashboard is active must reach the main screen and open its dialog")
}

// TestDashboardForwardsPermissionRequestToMainWithDialogOpen is the
// regression test for the hole left by the first pass at this fix: the
// dialog guard in handleDashboardMsg used to run before the forward-to-main
// branch, so a non-input message reaching it while a dashboard dialog
// (thread-create, remove-confirm) was open still fell into that guard's
// own "return r, nil" and was dropped — the exact permission-hang bug,
// just with a dashboard dialog on screen instead of the bare dashboard.
// The classification now happens first: non-input messages are forwarded
// to r.main before the dialog guard ever sees them.
func TestDashboardForwardsPermissionRequestToMainWithDialogOpen(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	model, _ := r.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	r = model.(*Root)
	r.dashboard = threads.New(r.com, &r.main.threadList)
	r.dashboard.SetSize(120, 40)
	r.active = screenDashboard
	r.dashboardDialog.OpenDialog(dialog.NewThreadCreate(r.com))
	require.True(t, r.dashboardDialog.HasDialogs(), "precondition: a dashboard dialog is open")

	perm := permission.PermissionRequest{ID: "perm-1", ToolCallID: "tc-1", ToolName: "bash"}
	model, _ = r.Update(pubsub.Event[permission.PermissionRequest]{Payload: perm})
	r = model.(*Root)

	require.Equal(t, screenDashboard, r.active, "the dashboard must stay on top")
	require.True(t, r.dashboardDialog.HasDialogs(), "the dashboard's own dialog must stay open")
	require.True(t, r.main.dialog.ContainsDialog(dialog.PermissionsID),
		"a permission request must reach the main screen and open its dialog even with a dashboard dialog open")
}

// TestDashboardMouseClickStaysOnDashboard proves the narrowed fix did not
// invert too much: mouse input is still the dashboard's own and must not
// be forwarded to r.main. A click lands on the dashboard's table/toolbar
// (handleDashboardMsg's tea.MouseClickMsg case) rather than falling
// through to the new forward-to-main branch, so no permissions dialog (or
// anything else main-side) is opened by it.
func TestDashboardMouseClickStaysOnDashboard(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	model, _ := r.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	r = model.(*Root)
	r.dashboard = threads.New(r.com, &r.main.threadList)
	r.dashboard.SetSize(120, 40)
	r.active = screenDashboard

	require.NotPanics(t, func() {
		model, _ = r.Update(tea.MouseClickMsg{X: 4, Y: 4, Button: tea.MouseLeft})
	})
	r = model.(*Root)

	require.Equal(t, screenDashboard, r.active)
	require.False(t, r.main.dialog.HasDialogs(),
		"a dashboard mouse click must not be forwarded to r.main")
}

// TestDashboardMouseClickThroughOpenDialogDoesNotReachDashboard proves the
// dialog guard's original job survives the restructuring: with a dashboard
// dialog open, a click must still go to that dialog, not click "through"
// the modal onto the table underneath. It clicks the same table-row
// coordinate twice — once with no dialog open, where it must move the
// dashboard's row selection, and once with a dialog open, where the
// selection (revealed by closing the dialog afterward) must be untouched.
func TestDashboardMouseClickThroughOpenDialogDoesNotReachDashboard(t *testing.T) {
	t.Parallel()

	rows := make([]proto.Thread, 5)
	for i := range rows {
		rows[i] = proto.Thread{
			ID: fmt.Sprintf("t%d", i), Name: fmt.Sprintf("thread %d", i),
			Kind: "thread", Status: "running",
		}
	}
	newDashboardRoot := func(t *testing.T) *Root {
		t.Helper()
		r := newTestRoot(t, true)
		model, _ := r.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		r = model.(*Root)
		r.main.threadList.Cache.Value = rows
		r.dashboard = threads.New(r.com, &r.main.threadList)
		r.dashboard.SetSize(120, 40)
		r.dashboard.RebuildItems()
		r.active = screenDashboard
		// HandleMouseClick hit-tests against zones computed by the last
		// Draw call; force one so the click below has something to hit.
		r.View()
		return r
	}

	// A click a few rows down the table (listRect starts at y=4 for this
	// size; row height is one line, so y=6 targets row index 2, which
	// differs from the default selection at row 0).
	rowClick := tea.MouseClickMsg{X: 5, Y: 6, Button: tea.MouseLeft}

	// Baseline: default selection, dialog closed.
	baseline := newDashboardRoot(t)
	before := baseline.View().Content

	// No dialog open: the click must reach the dashboard and move the
	// selection, so the rendered view changes.
	clicked := newDashboardRoot(t)
	model, _ := clicked.Update(rowClick)
	clicked = model.(*Root)
	afterDirectClick := clicked.View().Content
	require.NotEqual(t, before, afterDirectClick,
		"a dashboard click with no dialog open must reach the dashboard and move its selection")

	// Dialog open: the same click must go to the dialog instead. Closing
	// the dialog afterward reveals whether the selection moved.
	throughDialog := newDashboardRoot(t)
	throughDialog.dashboardDialog.OpenDialog(dialog.NewThreadCreate(throughDialog.com))
	model, _ = throughDialog.Update(rowClick)
	throughDialog = model.(*Root)
	throughDialog.dashboardDialog.CloseFrontDialog()
	afterClickThroughDialog := throughDialog.View().Content

	require.Equal(t, before, afterClickThroughDialog,
		"a click while a dashboard dialog is open must not reach the dashboard's table underneath")
}

// TestDashboardForwardsUntaggedAsyncResultToMain is the regression test
// for /thinking followed immediately by ctrl+e: modelSettingUpdatedMsg
// carries no uiOwnedMsg/MainScreenMsg tag, so while the dashboard was
// active it used to be dropped, leaving modelOperation.loading set for
// good — every later model/effort/provider change then answered "Model
// settings are already being updated". The dashboard now forwards it to
// r.main, the only *UI that could have dispatched it (no thread is ever
// attached while the dashboard is up).
func TestDashboardForwardsUntaggedAsyncResultToMain(t *testing.T) {
	t.Parallel()

	ws := &countingWorkspace{ready: true}
	r := &Root{com: newBusyUI(ws).com, main: newBusyUI(ws), active: screenMain, dashboardDialog: dialog.NewOverlay()}
	r.dashboard = threads.New(r.com, &r.main.threadList)

	generation, started := r.main.modelOperation.begin()
	require.True(t, started)
	require.True(t, r.main.modelOperation.isLoading())

	r.active = screenDashboard

	_, _ = r.Update(modelSettingUpdatedMsg{Info: "effort set", generation: generation})

	require.False(t, r.main.modelOperation.isLoading(),
		"an untagged async result dispatched by the main screen must not be dropped while the dashboard is active")
}

// TestNoThreadAttachedWhileDashboardActive pins the invariant the narrow
// fix in handleDashboardMsg relies on: leaveThread (the screenThread ->
// screenDashboard transition) detaches the thread before switching
// screens, and the other two entries into screenDashboard
// (showThreadsDashboardMsg, the handleKeyPress toggle) are only reached
// from screenMain, where nothing is attached. If this ever stops holding,
// handleDashboardMsg's forward-to-main branch must fail safe (keep
// dropping) rather than misdeliver to the wrong *UI — see its guard on
// r.attachment.thread == nil.
func TestNoThreadAttachedWhileDashboardActive(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	threadUI := New(r.com, "", false, WithEmbedded())
	r.attachment.thread = &threadAttachment{threadID: "s1", ui: threadUI, stop: func() {}, detach: func() {}}
	r.active = screenThread

	cmd := r.leaveThread()
	require.Equal(t, screenDashboard, r.active)
	require.Nil(t, r.attachment.thread,
		"no thread may remain attached once the dashboard becomes active")
	if cmd != nil {
		runBatchCmd(t, cmd)
	}
}
