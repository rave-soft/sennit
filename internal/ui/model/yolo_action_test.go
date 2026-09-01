package model

import (
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

	m, ws := newSettingsUI(newSettingsConfig())

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
