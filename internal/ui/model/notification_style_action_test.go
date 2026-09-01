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

func notificationStyleResult(t *testing.T, cmd tea.Cmd) notificationStyleSetMsg {
	t.Helper()

	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, nested := range batch {
			if result, ok := nested().(notificationStyleSetMsg); ok {
				return result
			}
		}
	}
	result, ok := msg.(notificationStyleSetMsg)
	require.True(t, ok)
	return result
}

func TestApplySettingsDialogAction_NotificationStyleLifecycle(t *testing.T) {
	t.Parallel()

	t.Run("duplicate begin warns without dispatching another write", func(t *testing.T) {
		t.Parallel()
		m, ws := newSettingsUI(&config.Config{Options: &config.Options{}})

		cmd, handled := m.applySettingsDialogAction(dialog.ActionSelectNotificationStyle{Style: "desktop"})
		require.True(t, handled)
		require.True(t, m.notificationStyle.isLoading())
		first := notificationStyleResult(t, cmd)
		require.Equal(t, uint64(1), first.generation)
		require.Equal(t, 1, ws.setConfigFieldCalls)

		cmd, handled = m.applySettingsDialogAction(dialog.ActionSelectNotificationStyle{Style: "bell"})
		require.True(t, handled)
		require.True(t, m.notificationStyle.isLoading())
		warning, ok := cmd().(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, util.InfoTypeWarn, warning.Type)
		require.Equal(t, "Notification settings are already being updated", warning.Msg)
		require.Equal(t, 1, ws.setConfigFieldCalls)

		m.updateSettings(first, nil)
		require.False(t, m.notificationStyle.isLoading())
	})

	t.Run("duplicate begin warns after configuration mutation", func(t *testing.T) {
		t.Parallel()

		for _, test := range []struct {
			name   string
			mutate func(*settingsTestWorkspace)
		}{
			{
				name: "configuration removed",
				mutate: func(ws *settingsTestWorkspace) {
					ws.cfg = nil
				},
			},
			{
				name: "options removed",
				mutate: func(ws *settingsTestWorkspace) {
					ws.cfg.Options = nil
				},
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				m, ws := newSettingsUI(&config.Config{Options: &config.Options{}})

				cmd, handled := m.applySettingsDialogAction(dialog.ActionSelectNotificationStyle{Style: "desktop"})
				require.True(t, handled)
				first := notificationStyleResult(t, cmd)
				require.True(t, m.notificationStyle.isLoading())
				require.Equal(t, 1, ws.setConfigFieldCalls)

				test.mutate(ws)
				cmd, handled = m.applySettingsDialogAction(dialog.ActionSelectNotificationStyle{Style: "bell"})
				require.True(t, handled)
				warning, ok := cmd().(util.InfoMsg)
				require.True(t, ok)
				require.Equal(t, util.InfoTypeWarn, warning.Type)
				require.Equal(t, "Notification settings are already being updated", warning.Msg)
				require.True(t, m.notificationStyle.isLoading())
				require.Equal(t, first.generation, m.notificationStyle.generation)
				require.Equal(t, 1, ws.setConfigFieldCalls)
			})
		}
	})

	t.Run("idle invalid configuration preserves zero state", func(t *testing.T) {
		t.Parallel()

		for _, cfg := range []*config.Config{nil, {}} {
			if cfg != nil {
				cfg.Options = nil
			}
			m, ws := newSettingsUI(cfg)

			cmd, handled := m.applySettingsDialogAction(dialog.ActionSelectNotificationStyle{Style: "desktop"})
			require.True(t, handled)
			require.Nil(t, cmd)
			require.False(t, m.notificationStyle.isLoading())
			require.Zero(t, m.notificationStyle.generation)
			require.Zero(t, ws.setConfigFieldCalls)
		}
	})

	t.Run("write error is consumed and permits a retry", func(t *testing.T) {
		t.Parallel()
		m, ws := newSettingsUI(&config.Config{Options: &config.Options{}})
		ws.setConfigFieldErr = errors.New("write failed")

		cmd, handled := m.applySettingsDialogAction(dialog.ActionSelectNotificationStyle{Style: "desktop"})
		require.True(t, handled)
		failed := notificationStyleResult(t, cmd)
		require.EqualError(t, failed.Err, "write failed")
		cmds, _ := m.updateSettings(failed, nil)
		require.False(t, m.notificationStyle.isLoading())
		require.Len(t, cmds, 1)

		ws.setConfigFieldErr = nil
		cmd, handled = m.applySettingsDialogAction(dialog.ActionSelectNotificationStyle{Style: "bell"})
		require.True(t, handled)
		require.True(t, m.notificationStyle.isLoading())
		retry := notificationStyleResult(t, cmd)
		require.Equal(t, failed.generation+1, retry.generation)
		require.Equal(t, 2, ws.setConfigFieldCalls)
	})
}
