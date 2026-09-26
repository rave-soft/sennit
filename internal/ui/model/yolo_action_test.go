package model

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/stretchr/testify/require"
)

func yoloResult(t *testing.T, cmd tea.Cmd) yoloToggledMsg {
	t.Helper()

	result, ok := cmd().(yoloToggledMsg)
	require.True(t, ok)
	return result
}

func TestApplySettingsDialogAction_YoloLifecycle(t *testing.T) {
	t.Parallel()

	m, ws := newSettingsUI(t, newSettingsConfig())

	cmd, handled := m.applySettingsDialogAction(dialog.ActionToggleYoloMode{})
	require.True(t, handled)
	first := yoloResult(t, cmd)
	require.True(t, m.yolo.isLoading())
	require.Equal(t, uint64(1), first.generation)
	require.Equal(t, 1, ws.permSetCalls)

	// Yolo has no configuration validation: PermissionSetSkipRequests is the
	// whole persistence operation. Its duplicate guard must therefore win even
	// if unrelated configuration changed while the first write was in flight.
	ws.cfg = nil
	cmd, handled = m.applySettingsDialogAction(dialog.ActionToggleYoloMode{})
	require.True(t, handled)
	warning, ok := cmd().(util.InfoMsg)
	require.True(t, ok)
	require.Equal(t, util.InfoTypeWarn, warning.Type)
	require.Equal(t, "Yolo mode is already being updated", warning.Msg)
	require.True(t, m.yolo.isLoading())
	require.Equal(t, first.generation, m.yolo.generation)
	require.Equal(t, 1, ws.permSetCalls)

	cmds, _ := m.updateSettings(first, nil)
	require.False(t, m.yolo.isLoading())
	require.Len(t, cmds, 1)

	cmd, handled = m.applySettingsDialogAction(dialog.ActionToggleYoloMode{})
	require.True(t, handled)
	retry := yoloResult(t, cmd)
	require.Equal(t, first.generation+1, retry.generation)
	require.Equal(t, 2, ws.permSetCalls)
}

// TestToggleYoloMode_SetSkipRequestsError pins the fix for a swallowed
// error: PermissionSetSkipRequests' result used to be discarded entirely
// in toggleYoloMode's Cmd, so a call that failed still produced a
// yoloToggledMsg with Enabled set to the desired (but never actually
// applied) state — and update_settings.go's existing yoloToggledMsg
// handler would have happily written that lie into the cache had Err
// stayed unset. This is the same display-lie class as round 1's
// yoloPermissionEnabledMsg finding: the cache must not claim the new
// state when the workspace call that was supposed to apply it failed.
func TestToggleYoloMode_SetSkipRequestsError(t *testing.T) {
	t.Parallel()

	m, ws := newSettingsUI(t, newSettingsConfig())
	m.wsCache.yoloCache.Set(false)

	setErr := errors.New("workspace unreachable")
	ws.permSetErr = setErr

	cmd := m.toggleYoloMode()
	require.NotNil(t, cmd)
	result := yoloResult(t, cmd)
	// Under SENNIT_TEST_WIRE=1, ws is wrapped in wsrpc.Loopback, and
	// setErr is an opaque error with no wire code (workspace.EncodeError
	// falls back to code "internal"): DecodeError on the far side
	// reconstructs it as a fresh errors.New(msg), so errors.Is against the
	// original sentinel value legitimately cannot succeed -- a real wire
	// hop would lose the same identity. Compare the message instead; that
	// survives every mode this test runs in.
	require.EqualError(t, result.Err, setErr.Error())
	require.True(t, result.Enabled, "toggling from off attempts to turn yolo on")

	cmds, _ := m.updateSettings(result, nil)

	reported := reportedErrorIn(t, cmds)
	require.Equal(t, util.InfoTypeError, reported.Type, "the failed toggle must be reported")

	require.False(t, m.wsCache.yoloModeCached(),
		"the cache must not claim yolo is on when PermissionSetSkipRequests failed")
}

// reportedErrorIn drains cmds, returning the first util.InfoMsg of type
// error it finds.
func reportedErrorIn(t *testing.T, cmds []tea.Cmd) util.InfoMsg {
	t.Helper()
	for _, cmd := range cmds {
		if cmd == nil {
			continue
		}
		if info, ok := cmd().(util.InfoMsg); ok && info.Type == util.InfoTypeError {
			return info
		}
	}
	return util.InfoMsg{}
}

// reportedErrorCount counts the error toasts among cmds.
func reportedErrorCount(cmds []tea.Cmd) int {
	n := 0
	for _, cmd := range cmds {
		if cmd == nil {
			continue
		}
		if info, ok := cmd().(util.InfoMsg); ok && info.Type == util.InfoTypeError {
			n++
		}
	}
	return n
}

// TestUpdateSettings_YoloPermissionEnabled_SkipOKGrantFails pins the fix
// for a display lie: PermissionSetSkipRequests and PermissionGrant are two
// separate workspace calls carried by one message, and when the first
// succeeds the workspace is already auto-approving everything regardless
// of what the second call does next. The handler must report yolo as
// enabled (cache + editor prompt) even though the grant itself failed,
// while still leaving the permission dialog open so the grant can be
// retried and the grant's own failure reported.
func TestUpdateSettings_YoloPermissionEnabled_SkipOKGrantFails(t *testing.T) {
	t.Parallel()

	m, _ := newSettingsUI(t, newSettingsConfig())
	const permID = "perm-skip-ok-grant-fails"

	m.permissionResponse.open(permID, false)
	permissionGeneration, started := m.permissionResponse.begin(permID)
	require.True(t, started)
	yoloGeneration, started := m.yolo.begin()
	require.True(t, started)
	m.dialog.OpenDialog(stubIDDialog{id: dialog.PermissionsID})

	grantErr := errors.New("workspace unreachable")
	cmds, _ := m.updateSettings(yoloPermissionEnabledMsg{
		uiOwned:              uiOwned{owner: m},
		GrantErr:             grantErr,
		Permission:           permID,
		permissionGeneration: permissionGeneration,
		yoloGeneration:       yoloGeneration,
	}, nil)

	reported := reportedErrorIn(t, cmds)
	require.Equal(t, util.InfoTypeError, reported.Type, "the grant failure must be reported")
	require.Contains(t, reported.Msg, grantErr.Error())

	require.True(t, m.wsCache.yoloModeCached(),
		"skip-requests succeeded, so yolo must be reported enabled regardless of the grant's own outcome")
	require.True(t, m.dialog.ContainsDialog(dialog.PermissionsID),
		"a failed grant (after a successful skip) must leave the dialog open for a retry")
	require.False(t, m.permissionResponse.loading,
		"the in-flight response must be cleared so a retry can begin")
	require.False(t, m.yolo.isLoading())
}

// TestUpdateSettings_YoloPermissionEnabled_SkipFails covers the other
// half: when PermissionSetSkipRequests itself failed (so PermissionGrant
// was never attempted — see the dispatching Cmd in dialog_actions.go),
// yolo must stay reported off.
func TestUpdateSettings_YoloPermissionEnabled_SkipFails(t *testing.T) {
	t.Parallel()

	m, _ := newSettingsUI(t, newSettingsConfig())
	const permID = "perm-skip-fails"

	m.permissionResponse.open(permID, false)
	permissionGeneration, started := m.permissionResponse.begin(permID)
	require.True(t, started)
	yoloGeneration, started := m.yolo.begin()
	require.True(t, started)
	m.dialog.OpenDialog(stubIDDialog{id: dialog.PermissionsID})

	skipErr := errors.New("workspace unreachable")
	cmds, _ := m.updateSettings(yoloPermissionEnabledMsg{
		uiOwned:              uiOwned{owner: m},
		SkipErr:              skipErr,
		Permission:           permID,
		permissionGeneration: permissionGeneration,
		yoloGeneration:       yoloGeneration,
	}, nil)

	reported := reportedErrorIn(t, cmds)
	require.Equal(t, util.InfoTypeError, reported.Type, "the skip failure must be reported")
	require.Contains(t, reported.Msg, skipErr.Error())
	require.Equal(t, 1, reportedErrorCount(cmds), "one failure is reported once")

	require.False(t, m.wsCache.yoloModeCached(), "yolo must stay reported off when skip-requests itself failed")
	require.True(t, m.dialog.ContainsDialog(dialog.PermissionsID),
		"a failed skip-requests must leave the dialog open for a retry")
	require.False(t, m.permissionResponse.loading,
		"the in-flight response must be cleared so a retry can begin")
	require.False(t, m.yolo.isLoading())
}
