package model

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/ui/attachments"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/stretchr/testify/require"
)

// settingsTestWorkspace extends countingWorkspace with the methods
// updateSettings's async branches call when their returned commands are
// actually run: Config (agentModelInitializedMsg's model-name lookup and
// importCopilotResult's provider check) and the model-init trio
// (InitCoderAgent/UpdateAgentModel/UpdatePreferredModel) that
// initAgentAndReportModel and importCopilotResult's follow-up command
// call through.
type settingsTestWorkspace struct {
	*countingWorkspace
	cfg *config.Config

	preferredCalls          []preferredModelCall
	initCoderAgentErr       error
	updateAgentModelErr     error
	updatePreferredModelErr error
}

func (w *settingsTestWorkspace) Config() *config.Config { return w.cfg }

func (w *settingsTestWorkspace) InitCoderAgent(context.Context) error {
	return w.initCoderAgentErr
}

func (w *settingsTestWorkspace) UpdateAgentModel(context.Context) error {
	return w.updateAgentModelErr
}

func (w *settingsTestWorkspace) UpdatePreferredModel(_ config.Scope, model config.SelectedModel) error {
	w.preferredCalls = append(w.preferredCalls, preferredModelCall{config.ScopeGlobal, model})
	return w.updatePreferredModelErr
}

// newSettingsUI builds a UI wired to a settingsTestWorkspace, reusing
// newBusyUI's fixture (chat/status/editor/dialog wiring) so setTheme,
// updateLayoutAndSize, and setEditorPrompt all have what they need.
func newSettingsUI(cfg *config.Config) (*UI, *settingsTestWorkspace) {
	ws := &settingsTestWorkspace{
		countingWorkspace: &countingWorkspace{ready: true},
		cfg:               cfg,
	}
	m := newBusyUI(ws)
	// newBusyUI wires editor.attachments with a nil renderer (attachments
	// aren't its concern); setTheme unconditionally dereferences it via
	// Renderer().SetStyles, so give it a real one here for the themeSetMsg
	// success/restore paths this file exercises.
	m.editor.attachments = attachments.New(
		attachments.NewRenderer(lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle()),
		attachments.Keymap{},
	)
	return m, ws
}

func newSettingsConfig() *config.Config {
	return &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}
}

// stubIDDialog is a minimal [dialog.Dialog] used only so CloseDialog/
// ContainsDialog have something with a matching ID to act on.
type stubIDDialog struct{ id string }

func (d stubIDDialog) ID() string                               { return d.id }
func (d stubIDDialog) HandleMsg(tea.Msg) dialog.Action          { return nil }
func (d stubIDDialog) Draw(uv.Screen, uv.Rectangle) *tea.Cursor { return nil }

func firstMsg(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

func TestUpdateSettings_ProviderConfiguredResult(t *testing.T) {
	t.Parallel()

	t.Run("stale generation is dropped", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.modelOperationGeneration = 5
		m.ops.modelOperationLoading = true

		cmds, done := m.updateSettings(providerConfiguredResult{generation: 4}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
		require.True(t, m.ops.modelOperationLoading, "stale reply must not touch loading state")
	})

	t.Run("error reports and clears loading", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.modelOperationGeneration = 1
		m.ops.modelOperationLoading = true
		wantErr := errors.New("boom")

		cmds, done := m.updateSettings(providerConfiguredResult{Err: wantErr, generation: 1}, nil)

		require.False(t, done)
		require.False(t, m.ops.modelOperationLoading)
		require.Len(t, cmds, 1)
		got, ok := firstMsg(cmds[0]).(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, util.InfoTypeError, got.Type)
		require.Equal(t, "boom", got.Msg)
	})

	t.Run("success dispatches init command", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.modelOperationGeneration = 1

		cmds, done := m.updateSettings(providerConfiguredResult{
			Model:      config.SelectedModel{Provider: "p", Model: "m"},
			generation: 1,
		}, nil)

		require.False(t, done)
		// One command drives model/agent init; the other refreshes the
		// sidebar's cached account label for the now-configured provider
		// (see account_label.go) — harmless for the common "just signed
		// in" case, since that cache is a no-op for a single-account
		// provider.
		require.Len(t, cmds, 2)
		require.NotNil(t, cmds[0])
		require.NotNil(t, cmds[1])
	})
}

func TestUpdateSettings_ModelSelectResult(t *testing.T) {
	t.Parallel()

	t.Run("stale generation is dropped", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.modelOperationGeneration = 2

		cmds, done := m.updateSettings(modelSelectResult{generation: 1}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
	})

	t.Run("error reports and clears loading", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.modelOperationGeneration = 1
		m.ops.modelOperationLoading = true
		wantErr := errors.New("select failed")

		cmds, done := m.updateSettings(modelSelectResult{Err: wantErr, generation: 1}, nil)

		require.False(t, done)
		require.False(t, m.ops.modelOperationLoading)
		require.Len(t, cmds, 1)
		got, ok := firstMsg(cmds[0]).(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, "select failed", got.Msg)
	})

	t.Run("success dispatches init command", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.modelOperationGeneration = 3

		cmds, done := m.updateSettings(modelSelectResult{
			Onboarding: true,
			Model:      config.SelectedModel{Provider: "p", Model: "m"},
			generation: 3,
		}, nil)

		require.False(t, done)
		// As with providerConfiguredResult above: one command drives
		// model/agent init, the other refreshes the sidebar's account
		// label, since selecting a model can move to a provider whose
		// label the cache has never held.
		require.Len(t, cmds, 2)
		require.NotNil(t, cmds[0])
		require.NotNil(t, cmds[1])
	})
}

func TestUpdateSettings_AgentModelInitializedMsg(t *testing.T) {
	t.Parallel()

	t.Run("stale generation is dropped", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.modelOperationGeneration = 2
		m.ops.modelOperationLoading = true

		cmds, done := m.updateSettings(agentModelInitializedMsg{generation: 1}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
		require.True(t, m.ops.modelOperationLoading)
	})

	t.Run("error reports and clears loading", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.modelOperationGeneration = 1
		m.ops.modelOperationLoading = true
		wantErr := errors.New("init failed")

		cmds, done := m.updateSettings(agentModelInitializedMsg{Err: wantErr, generation: 1}, nil)

		require.False(t, done)
		require.False(t, m.ops.modelOperationLoading)
		require.Len(t, cmds, 1)
		got, ok := firstMsg(cmds[0]).(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, "init failed", got.Msg)
	})

	t.Run("onboarding success switches to landing and reports the catalog name", func(t *testing.T) {
		t.Parallel()
		cfg := newSettingsConfig()
		cfg.Providers.Set("p", config.ProviderConfig{
			ID:     "p",
			Models: []catwalk.Model{{ID: "m", Name: "Pretty Model"}},
		})
		m, _ := newSettingsUI(cfg)
		m.ops.modelOperationGeneration = 1
		m.ops.modelOperationLoading = true
		m.state = uiChat

		cmds, done := m.updateSettings(agentModelInitializedMsg{
			Onboarding: true,
			Model:      config.SelectedModel{Provider: "p", Model: "m"},
			generation: 1,
		}, nil)

		require.False(t, done)
		require.False(t, m.ops.modelOperationLoading)
		require.Equal(t, uiLanding, m.state)
		require.Len(t, cmds, 2)
		info, ok := firstMsg(cmds[0]).(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, "Model changed to Pretty Model", info.Msg)
		require.IsType(t, agentModelChangedMsg{}, firstMsg(cmds[1]))
	})

	t.Run("non-onboarding success without a catalog match reports the raw model id", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.modelOperationGeneration = 1
		m.state = uiChat

		cmds, done := m.updateSettings(agentModelInitializedMsg{
			Model:      config.SelectedModel{Provider: "p", Model: "raw-id"},
			generation: 1,
		}, nil)

		require.False(t, done)
		require.Equal(t, uiChat, m.state, "non-onboarding must not switch state")
		info, ok := firstMsg(cmds[0]).(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, "Model changed to raw-id", info.Msg)
	})
}

func TestUpdateSettings_ModelSettingUpdatedMsg(t *testing.T) {
	t.Parallel()

	t.Run("stale generation is dropped", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.modelOperationGeneration = 2

		cmds, done := m.updateSettings(modelSettingUpdatedMsg{generation: 1}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
	})

	t.Run("error reports", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.modelOperationGeneration = 1
		wantErr := errors.New("setting failed")

		cmds, _ := m.updateSettings(modelSettingUpdatedMsg{Err: wantErr, generation: 1}, nil)

		require.Len(t, cmds, 1)
		got, ok := firstMsg(cmds[0]).(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, util.InfoTypeError, got.Type)
	})

	t.Run("success reports info", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.modelOperationGeneration = 1

		cmds, _ := m.updateSettings(modelSettingUpdatedMsg{Info: "effort set", generation: 1}, nil)

		require.Len(t, cmds, 1)
		got, ok := firstMsg(cmds[0]).(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, util.InfoTypeInfo, got.Type)
		require.Equal(t, "effort set", got.Msg)
	})
}

func TestUpdateSettings_TransparentToggledMsg(t *testing.T) {
	t.Parallel()

	t.Run("stale generation is dropped", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.transparentGeneration = 2
		m.ops.transparentLoading = true

		cmds, done := m.updateSettings(transparentToggledMsg{generation: 1}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
		require.True(t, m.ops.transparentLoading)
	})

	t.Run("error reports and leaves transparency unchanged", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.transparentGeneration = 1
		m.ops.transparentLoading = true
		m.lay.isTransparent = false

		cmds, _ := m.updateSettings(transparentToggledMsg{Err: errors.New("nope"), Enabled: true, generation: 1}, nil)

		require.False(t, m.ops.transparentLoading)
		require.False(t, m.lay.isTransparent)
		require.Len(t, cmds, 1)
	})

	t.Run("success flips transparency and closes the commands dialog", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.transparentGeneration = 1
		m.dialog.OpenDialog(stubIDDialog{id: dialog.CommandsID})

		cmds, _ := m.updateSettings(transparentToggledMsg{Enabled: true, generation: 1}, nil)

		require.True(t, m.lay.isTransparent)
		require.False(t, m.dialog.ContainsDialog(dialog.CommandsID))
		require.Empty(t, cmds)
	})
}

func TestUpdateSettings_ThemeSetMsg(t *testing.T) {
	t.Parallel()

	t.Run("stale generation is dropped", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.themeGeneration = 2
		m.ops.themeLive = "steel-teal"

		cmds, done := m.updateSettings(themeSetMsg{generation: 1}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
		require.Equal(t, "steel-teal", m.ops.themeLive)
	})

	t.Run("error restores the previous palette and reports", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.themeGeneration = 1
		m.ops.themeLive = "graphite-amber"

		cmds, _ := m.updateSettings(themeSetMsg{
			Err:        errors.New("write failed"),
			Previous:   "steel-teal",
			generation: 1,
		}, nil)

		require.Equal(t, "steel-teal", m.ops.themeLive, "setTheme(Previous) must run")
		require.NotEmpty(t, cmds)
		last := cmds[len(cmds)-1]
		got, ok := firstMsg(last).(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, util.InfoTypeError, got.Type)
	})

	t.Run("success reports the new palette name", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.themeGeneration = 1

		cmds, _ := m.updateSettings(themeSetMsg{ID: "steel-teal", generation: 1}, nil)

		require.Len(t, cmds, 1)
		got, ok := firstMsg(cmds[0]).(util.InfoMsg)
		require.True(t, ok)
		require.Contains(t, got.Msg, "Theme set to:")
	})
}

func TestUpdateSettings_CompactModeToggledMsg(t *testing.T) {
	t.Parallel()

	t.Run("stale generation is dropped", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.compactModeGeneration = 2
		m.ops.compactModeLoading = true

		cmds, done := m.updateSettings(compactModeToggledMsg{generation: 1}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
		require.True(t, m.ops.compactModeLoading)
	})

	t.Run("error reports and leaves compact mode unchanged", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.compactModeGeneration = 1
		m.ops.compactModeLoading = true
		m.lay.forceCompactMode = false

		cmds, _ := m.updateSettings(compactModeToggledMsg{Err: errors.New("nope"), Enabled: true, generation: 1}, nil)

		require.False(t, m.ops.compactModeLoading)
		require.False(t, m.lay.forceCompactMode)
		require.Len(t, cmds, 1)
	})

	t.Run("success flips compact mode and closes the commands dialog", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.compactModeGeneration = 1
		m.dialog.OpenDialog(stubIDDialog{id: dialog.CommandsID})

		cmds, _ := m.updateSettings(compactModeToggledMsg{Enabled: true, generation: 1}, nil)

		require.True(t, m.lay.forceCompactMode)
		require.True(t, m.lay.isCompact)
		require.False(t, m.dialog.ContainsDialog(dialog.CommandsID))
		require.Empty(t, cmds)
	})
}

func TestUpdateSettings_NotificationStyleSetMsg(t *testing.T) {
	t.Parallel()

	t.Run("stale generation is dropped", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.notificationGeneration = 2
		m.ops.notificationLoading = true

		cmds, done := m.updateSettings(notificationStyleSetMsg{generation: 1}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
		require.True(t, m.ops.notificationLoading)
	})

	t.Run("error reports", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.notificationGeneration = 1
		m.ops.notificationLoading = true

		cmds, _ := m.updateSettings(notificationStyleSetMsg{Err: errors.New("bad"), generation: 1}, nil)

		require.False(t, m.ops.notificationLoading)
		require.Len(t, cmds, 1)
	})

	t.Run("success closes the dialog and reports the new style", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.notificationGeneration = 1
		m.dialog.OpenDialog(stubIDDialog{id: dialog.NotificationsID})

		cmds, _ := m.updateSettings(notificationStyleSetMsg{Style: "desktop", generation: 1}, nil)

		require.False(t, m.dialog.ContainsDialog(dialog.NotificationsID))
		require.Len(t, cmds, 1)
		got, ok := firstMsg(cmds[0]).(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, "Notifications set to: desktop", got.Msg)
	})
}

func TestUpdateSettings_PermissionResponseMsg(t *testing.T) {
	t.Parallel()

	t.Run("stale generation is dropped", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.permissionGeneration = 2
		m.ops.permissionID = "perm-1"
		m.ops.permissionLoading = true

		cmds, done := m.updateSettings(permissionResponseMsg{Permission: "perm-1", generation: 1}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
		require.True(t, m.ops.permissionLoading)
	})

	t.Run("mismatched permission id is dropped", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.permissionGeneration = 1
		m.ops.permissionID = "perm-1"
		m.ops.permissionLoading = true

		cmds, done := m.updateSettings(permissionResponseMsg{Permission: "perm-2", generation: 1}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
		require.True(t, m.ops.permissionLoading)
	})

	t.Run("refused closes the dialog and reports an error", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.permissionGeneration = 1
		m.ops.permissionID = "perm-1"
		m.ops.permissionLoading = true
		m.dialog.OpenDialog(stubIDDialog{id: dialog.PermissionsID})

		cmds, _ := m.updateSettings(permissionResponseMsg{Permission: "perm-1", Accepted: false, generation: 1}, nil)

		require.False(t, m.ops.permissionLoading)
		require.False(t, m.dialog.ContainsDialog(dialog.PermissionsID))
		require.Len(t, cmds, 1)
		got, ok := firstMsg(cmds[0]).(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, util.InfoTypeError, got.Type)
	})

	t.Run("accepted closes the dialog without reporting anything", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.permissionGeneration = 1
		m.ops.permissionID = "perm-1"
		m.ops.permissionLoading = true
		m.dialog.OpenDialog(stubIDDialog{id: dialog.PermissionsID})

		cmds, _ := m.updateSettings(permissionResponseMsg{Permission: "perm-1", Accepted: true, generation: 1}, nil)

		require.False(t, m.ops.permissionLoading)
		require.False(t, m.dialog.ContainsDialog(dialog.PermissionsID))
		require.Empty(t, cmds)
	})
}

func TestUpdateSettings_YoloToggledMsg(t *testing.T) {
	t.Parallel()

	t.Run("stale generation is dropped", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.yoloGeneration = 2
		m.ops.yoloLoading = true

		cmds, done := m.updateSettings(yoloToggledMsg{generation: 1}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
		require.True(t, m.ops.yoloLoading)
	})

	t.Run("error reports", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.yoloGeneration = 1
		m.ops.yoloLoading = true

		cmds, _ := m.updateSettings(yoloToggledMsg{Err: errors.New("nope"), generation: 1}, nil)

		require.False(t, m.ops.yoloLoading)
		require.Len(t, cmds, 1)
	})

	t.Run("enabling reports enabled", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.yoloGeneration = 1

		cmds, _ := m.updateSettings(yoloToggledMsg{Enabled: true, generation: 1}, nil)

		require.True(t, m.wsCache.yoloCache.Value)
		got, ok := firstMsg(cmds[0]).(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, "Yolo mode enabled", got.Msg)
	})

	t.Run("disabling reports disabled", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.yoloGeneration = 1

		cmds, _ := m.updateSettings(yoloToggledMsg{Enabled: false, generation: 1}, nil)

		require.False(t, m.wsCache.yoloCache.Value)
		got, ok := firstMsg(cmds[0]).(util.InfoMsg)
		require.True(t, ok)
		require.Equal(t, "Yolo mode disabled", got.Msg)
	})
}

func TestUpdateSettings_NotificationSentMsg(t *testing.T) {
	t.Parallel()

	m, _ := newSettingsUI(newSettingsConfig())

	cmds, done := m.updateSettings(notificationSentMsg{}, nil)

	require.False(t, done)
	require.Empty(t, cmds)
}

func TestUpdateSettings_ImportCopilotResult(t *testing.T) {
	t.Parallel()

	t.Run("stale generation is dropped", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.modelOperationGeneration = 2

		cmds, done := m.updateSettings(importCopilotResult{generation: 1}, nil)

		require.False(t, done)
		require.Empty(t, cmds)
	})

	t.Run("provider not configured opens the auth dialog", func(t *testing.T) {
		t.Parallel()
		m, _ := newSettingsUI(newSettingsConfig())
		m.ops.modelOperationGeneration = 1
		m.ops.modelOperationLoading = true
		m.dialog.OpenDialog(stubIDDialog{id: dialog.ModelsID})

		cmds, done := m.updateSettings(importCopilotResult{
			providerID: "github-copilot",
			generation: 1,
		}, nil)

		require.True(t, done)
		require.False(t, m.ops.modelOperationLoading)
		require.False(t, m.dialog.ContainsDialog(dialog.ModelsID))
		// openAuthenticationDialog's API-key path opens the dialog
		// synchronously and returns a nil cmd (no verification spinner to
		// arm yet), so cmds stays empty.
		require.Empty(t, cmds)
		require.True(t, m.dialog.ContainsDialog(dialog.APIKeyInputID))
	})

	t.Run("provider configured dispatches UpdatePreferredModel", func(t *testing.T) {
		t.Parallel()
		cfg := newSettingsConfig()
		cfg.Providers.Set("github-copilot", config.ProviderConfig{ID: "github-copilot"})
		m, ws := newSettingsUI(cfg)
		m.ops.modelOperationGeneration = 1

		cmds, done := m.updateSettings(importCopilotResult{
			providerID: "github-copilot",
			model:      config.SelectedModel{Provider: "github-copilot", Model: "gpt"},
			generation: 1,
		}, nil)

		require.True(t, done)
		require.Len(t, cmds, 1)
		msg := firstMsg(cmds[0])
		result, ok := msg.(modelSelectResult)
		require.True(t, ok)
		require.NoError(t, result.Err)
		require.Equal(t, "gpt", result.Model.Model)
		require.Len(t, ws.preferredCalls, 1)
		require.Equal(t, "gpt", ws.preferredCalls[0].model.Model)
	})

	t.Run("provider configured but UpdatePreferredModel fails reports the error via the result message", func(t *testing.T) {
		t.Parallel()
		cfg := newSettingsConfig()
		cfg.Providers.Set("github-copilot", config.ProviderConfig{ID: "github-copilot"})
		m, ws := newSettingsUI(cfg)
		ws.updatePreferredModelErr = errors.New("disk full")
		m.ops.modelOperationGeneration = 1

		cmds, done := m.updateSettings(importCopilotResult{
			providerID: "github-copilot",
			model:      config.SelectedModel{Provider: "github-copilot", Model: "gpt"},
			generation: 1,
		}, nil)

		require.True(t, done)
		msg := firstMsg(cmds[0])
		result, ok := msg.(modelSelectResult)
		require.True(t, ok)
		require.EqualError(t, result.Err, "disk full")
	})
}
