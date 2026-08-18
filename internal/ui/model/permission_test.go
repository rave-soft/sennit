package model

import (
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// newTestUIForPermissions builds a UI with a chat, dialog overlay, and
// common context sufficient to exercise handlePermissionNotification.
func newTestUIForPermissions() *UI {
	u := newTestUI()
	u.dialog = dialog.NewOverlay()
	return u
}

// newTestUIForOpeningPermissions is newTestUIForPermissions plus the
// config openPermissionsDialog reads for its diff mode. The tests that
// build the dialog by hand do not need it; the ones that go through
// openPermissionsDialog do.
func newTestUIForOpeningPermissions(t *testing.T) *UI {
	t.Helper()
	u := newTestUIForPermissions()
	// Only the workspace is swapped in: the rest of the Common (styles in
	// particular) is what dialog.NewPermissions renders through.
	u.com.Workspace = &testWorkspace{cfg: &config.Config{
		Options: &config.Options{TUI: &config.TUIOptions{}},
	}}
	return u
}

func TestHandlePermissionNotification_RemoteGrantClosesDialog(t *testing.T) {
	t.Parallel()

	u := newTestUIForPermissions()
	perm := permission.PermissionRequest{
		ID:         "perm-1",
		ToolCallID: "tool-call-X",
		ToolName:   "bash",
	}
	u.dialog.OpenDialogWithGrace(dialog.NewPermissions(u.com, perm))
	require.True(t, u.dialog.ContainsDialog(dialog.PermissionsID))

	u.handlePermissionNotification(permission.PermissionNotification{
		ToolCallID: "tool-call-X",
		Granted:    true,
	})

	require.False(t, u.dialog.ContainsDialog(dialog.PermissionsID),
		"granted notification should close matching permissions dialog")
}

func TestHandlePermissionNotification_RemoteDenyClosesDialog(t *testing.T) {
	t.Parallel()

	u := newTestUIForPermissions()
	perm := permission.PermissionRequest{
		ID:         "perm-2",
		ToolCallID: "tool-call-Y",
	}
	u.dialog.OpenDialogWithGrace(dialog.NewPermissions(u.com, perm))

	u.handlePermissionNotification(permission.PermissionNotification{
		ToolCallID: "tool-call-Y",
		Denied:     true,
	})

	require.False(t, u.dialog.ContainsDialog(dialog.PermissionsID),
		"denied notification should close matching permissions dialog")
}

func TestHandlePermissionNotification_InitialPendingDoesNotClose(t *testing.T) {
	t.Parallel()

	u := newTestUIForPermissions()
	perm := permission.PermissionRequest{
		ID:         "perm-3",
		ToolCallID: "tool-call-Z",
	}
	u.dialog.OpenDialogWithGrace(dialog.NewPermissions(u.com, perm))

	// The initial notification published by permission.Request is
	// neither granted nor denied; it must not dismiss the dialog.
	u.handlePermissionNotification(permission.PermissionNotification{
		ToolCallID: "tool-call-Z",
	})

	require.True(t, u.dialog.ContainsDialog(dialog.PermissionsID),
		"initial pending notification must not close the dialog")
}

func TestHandlePermissionNotification_DifferentToolCallIDDoesNotClose(t *testing.T) {
	t.Parallel()

	u := newTestUIForPermissions()
	perm := permission.PermissionRequest{
		ID:         "perm-4",
		ToolCallID: "tool-call-A",
	}
	u.dialog.OpenDialogWithGrace(dialog.NewPermissions(u.com, perm))

	u.handlePermissionNotification(permission.PermissionNotification{
		ToolCallID: "tool-call-B",
		Granted:    true,
	})

	require.True(t, u.dialog.ContainsDialog(dialog.PermissionsID),
		"notification for unrelated tool call must not close the dialog")
}

// A thread's permission request reaches the drilled-in UI twice — once
// through the thread's own event pump, once through the relay into the
// parent's stream. The duplicate must not disturb the dialog already
// standing for it.
//
// Reopening on the duplicate is what made the prompt unanswerable: it
// bumps the generation an in-flight answer is matched against, so the
// answer is dropped, and the fresh dialog then stands for a request the
// service has already decided and will refuse to decide again.
func TestOpenPermissionsDialog_DuplicateRequestIsIgnored(t *testing.T) {
	t.Parallel()

	u := newTestUIForOpeningPermissions(t)
	perm := permission.PermissionRequest{
		ID:         "perm-dup",
		ToolCallID: "tool-call-dup",
		ToolName:   "bash",
	}

	require.Nil(t, u.openPermissionsDialog(perm))
	require.True(t, u.dialog.ContainsDialog(dialog.PermissionsID))
	gen := u.ops.permissionGeneration

	// The user answers: the response path claims the current generation.
	u.ops.permissionLoading = true

	// The second copy of the same request arrives now.
	require.Nil(t, u.openPermissionsDialog(perm))

	require.Equal(t, gen, u.ops.permissionGeneration,
		"a duplicate must not bump the generation an in-flight answer is matched against")
	require.True(t, u.ops.permissionLoading,
		"a duplicate must not clear the in-flight answer's loading state")
	require.True(t, u.dialog.ContainsDialog(dialog.PermissionsID))
}

// A genuinely different request still replaces whatever is open: the
// dedup is keyed on the request id, not on "a permissions dialog exists".
func TestOpenPermissionsDialog_DifferentRequestReopens(t *testing.T) {
	t.Parallel()

	u := newTestUIForOpeningPermissions(t)
	first := permission.PermissionRequest{ID: "perm-1", ToolCallID: "tc-1", ToolName: "bash"}
	second := permission.PermissionRequest{ID: "perm-2", ToolCallID: "tc-2", ToolName: "edit"}

	require.Nil(t, u.openPermissionsDialog(first))
	gen := u.ops.permissionGeneration

	require.Nil(t, u.openPermissionsDialog(second))
	require.Greater(t, u.ops.permissionGeneration, gen,
		"a new request must claim a new generation")
	require.Equal(t, "perm-2", u.ops.permissionID)
	require.True(t, u.dialog.ContainsDialog(dialog.PermissionsID))
}

// Once the dialog is gone, the same id may legitimately open a new one:
// the guard must key on a dialog actually being open, not on the id alone.
func TestOpenPermissionsDialog_SameIDReopensAfterClose(t *testing.T) {
	t.Parallel()

	u := newTestUIForOpeningPermissions(t)
	perm := permission.PermissionRequest{ID: "perm-1", ToolCallID: "tc-1", ToolName: "bash"}

	require.Nil(t, u.openPermissionsDialog(perm))
	gen := u.ops.permissionGeneration

	u.dialog.CloseDialog(dialog.PermissionsID)
	require.False(t, u.dialog.ContainsDialog(dialog.PermissionsID))

	require.Nil(t, u.openPermissionsDialog(perm))
	require.Greater(t, u.ops.permissionGeneration, gen)
	require.True(t, u.dialog.ContainsDialog(dialog.PermissionsID))
}

// A refused answer means no permission service still has this request
// pending: it was already decided, or the run that raised it ended.
// Leaving the dialog up was the worse half of that failure -- the prompt
// could be neither answered nor dismissed, and the session sat behind a
// dead modal.
func TestPermissionResponse_RefusedAnswerClosesTheDialog(t *testing.T) {
	t.Parallel()

	u := newTestUIForOpeningPermissions(t)
	perm := permission.PermissionRequest{ID: "perm-1", ToolName: "bash", Action: "execute"}
	u.openPermissionsDialog(perm)
	require.NotNil(t, u.dialog.Dialog(dialog.PermissionsID), "precondition: the prompt is on screen")

	_, _ = u.updateSettings(permissionResponseMsg{
		Accepted:   false,
		Permission: perm.ID,
		generation: u.ops.permissionGeneration,
	}, nil)

	require.Nil(t, u.dialog.Dialog(dialog.PermissionsID),
		"a prompt nothing is waiting on must not stay on screen")
	require.False(t, u.ops.permissionLoading)
}

// An accepted answer still closes it, and does not report an error.
func TestPermissionResponse_AcceptedAnswerClosesTheDialog(t *testing.T) {
	t.Parallel()

	u := newTestUIForOpeningPermissions(t)
	perm := permission.PermissionRequest{ID: "perm-1", ToolName: "bash", Action: "execute"}
	u.openPermissionsDialog(perm)

	_, _ = u.updateSettings(permissionResponseMsg{
		Accepted:   true,
		Permission: perm.ID,
		generation: u.ops.permissionGeneration,
	}, nil)

	require.Nil(t, u.dialog.Dialog(dialog.PermissionsID))
}

// A stale response -- one whose generation no longer matches, because a
// newer request replaced it -- must not close the prompt now on screen.
func TestPermissionResponse_StaleAnswerLeavesTheDialogAlone(t *testing.T) {
	t.Parallel()

	u := newTestUIForOpeningPermissions(t)
	perm := permission.PermissionRequest{ID: "perm-1", ToolName: "bash", Action: "execute"}
	u.openPermissionsDialog(perm)

	_, _ = u.updateSettings(permissionResponseMsg{
		Accepted:   false,
		Permission: perm.ID,
		generation: u.ops.permissionGeneration - 1,
	}, nil)

	require.NotNil(t, u.dialog.Dialog(dialog.PermissionsID))
}
