package model

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/stretchr/testify/require"
)

func transparencyResult(t *testing.T, cmd tea.Cmd) transparentToggledMsg {
	t.Helper()

	result, ok := cmd().(transparentToggledMsg)
	require.True(t, ok)
	return result
}

func TestApplySettingsDialogAction_TransparencyLifecycle(t *testing.T) {
	t.Parallel()

	t.Run("duplicate begin warns after configuration removal without another write", func(t *testing.T) {
		t.Parallel()
		m, ws := newSettingsUI(&config.Config{Options: &config.Options{TUI: &config.TUIOptions{}}})

		cmd, handled := m.applySettingsDialogAction(dialog.ActionToggleTransparentBackground{})
		require.True(t, handled)
		first := transparencyResult(t, cmd)
		require.True(t, m.transparency.isLoading())
		require.Equal(t, uint64(1), first.generation)
		require.Equal(t, 1, ws.setConfigFieldCalls)

		ws.cfg = nil
		cmd, handled = m.applySettingsDialogAction(dialog.ActionToggleTransparentBackground{})
		require.True(t, handled)
		warning, ok := cmd().(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, util.InfoTypeWarn, warning.Type)
		require.Equal(t, "Transparency is already being updated", warning.Msg)
		require.True(t, m.transparency.isLoading())
		require.Equal(t, first.generation, m.transparency.generation)
		require.Equal(t, 1, ws.setConfigFieldCalls)
	})

	t.Run("idle invalid configuration preserves zero state", func(t *testing.T) {
		t.Parallel()
		m, ws := newSettingsUI(nil)

		cmd, handled := m.applySettingsDialogAction(dialog.ActionToggleTransparentBackground{})

		require.True(t, handled)
		require.NotNil(t, cmd)
		require.False(t, m.transparency.isLoading())
		require.Zero(t, m.transparency.generation)
		require.Zero(t, ws.setConfigFieldCalls)
	})

	t.Run("write error is consumed and permits a retry", func(t *testing.T) {
		t.Parallel()
		m, ws := newSettingsUI(&config.Config{Options: &config.Options{TUI: &config.TUIOptions{}}})
		ws.setConfigFieldErr = errors.New("write failed")

		cmd, handled := m.applySettingsDialogAction(dialog.ActionToggleTransparentBackground{})
		require.True(t, handled)
		failed := transparencyResult(t, cmd)
		require.EqualError(t, failed.Err, "write failed")
		cmds, _ := m.updateSettings(failed, nil)
		require.False(t, m.transparency.isLoading())
		require.Len(t, cmds, 1)

		ws.setConfigFieldErr = nil
		cmd, handled = m.applySettingsDialogAction(dialog.ActionToggleTransparentBackground{})
		require.True(t, handled)
		retry := transparencyResult(t, cmd)
		require.True(t, m.transparency.isLoading())
		require.Equal(t, failed.generation+1, retry.generation)
		require.Equal(t, 2, ws.setConfigFieldCalls)
	})
}
