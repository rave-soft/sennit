package model

import (
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/ui/dialog"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/rave-soft/braid/internal/ui/util"
)

// updateSettings handles the dialog-result and settings branches of
// UI.Update: provider/model selection, theme, transparency, compact mode,
// notification style, permission responses, yolo toggling, notification
// delivery, and Copilot import. It is called from Update's message-type
// switch and shares that switch's cmds accumulator.
//
// The second return value reports whether a branch below took one of
// Update's early-return paths (return m, tea.Batch(cmds...)): when true,
// the caller must return immediately with the returned cmds, bypassing the
// rest of Update's tail (the focus/placeholder switch, stale-workspace
// refresh, and attachment update) exactly as the original inline case did.
// When false, a branch fell through instead, and the caller must continue
// running that tail with the returned cmds, exactly as falling out of the
// original case body would.
func (m *UI) updateSettings(msg tea.Msg, cmds []tea.Cmd) ([]tea.Cmd, bool) {
	switch msg := msg.(type) {
	case providerConfiguredResult:
		if msg.generation != m.ops.modelOperationGeneration {
			break
		}
		if msg.Err != nil {
			m.ops.modelOperationLoading = false
			cmds = append(cmds, util.ReportError(msg.Err))
			break
		}
		cmds = append(cmds, m.initAgentAndReportModel(true, msg.Model, msg.generation))

	case modelSelectResult:
		if msg.generation != m.ops.modelOperationGeneration {
			break
		}
		if msg.Err != nil {
			m.ops.modelOperationLoading = false
			cmds = append(cmds, util.ReportError(msg.Err))
			break
		}
		cmds = append(cmds, m.initAgentAndReportModel(msg.Onboarding, msg.Model, msg.generation))

	case agentModelInitializedMsg:
		if msg.generation != m.ops.modelOperationGeneration {
			break
		}
		m.ops.modelOperationLoading = false
		if msg.Err != nil {
			cmds = append(cmds, util.ReportError(msg.Err))
			break
		}
		if msg.Onboarding {
			m.setState(uiLanding, uiFocusEditor)
		}
		modelName := msg.Model.Model
		if cfg := m.com.Config(); cfg != nil {
			if selected := cfg.GetModel(msg.Model.Provider, msg.Model.Model); selected != nil && selected.Name != "" {
				modelName = selected.Name
			}
		}
		cmds = append(cmds, util.ReportInfo(fmt.Sprintf("Model changed to %s", modelName)), func() tea.Msg { return agentModelChangedMsg{} })

	case modelSettingUpdatedMsg:
		if msg.generation != m.ops.modelOperationGeneration {
			break
		}
		m.ops.modelOperationLoading = false
		if msg.Err != nil {
			cmds = append(cmds, util.ReportError(msg.Err))
		} else {
			cmds = append(cmds, util.ReportInfo(msg.Info))
		}

	case transparentToggledMsg:
		if msg.generation != m.ops.transparentGeneration {
			break
		}
		m.ops.transparentLoading = false
		if msg.Err != nil {
			cmds = append(cmds, util.ReportError(msg.Err))
			break
		}
		m.isTransparent = msg.Enabled
		m.dialog.CloseDialog(dialog.CommandsID)

	case themeSetMsg:
		if msg.generation != m.ops.themeGeneration {
			break
		}
		if msg.Err != nil {
			// The palette was swapped optimistically; put it back so what
			// is on screen matches what is on disk.
			m.setTheme(msg.Previous)
			cmds = append(cmds, util.ReportError(msg.Err))
			break
		}
		cmds = append(cmds, util.ReportInfo("Theme set to: "+styles.PaletteByID(msg.ID).Name))

	case compactModeToggledMsg:
		if msg.generation != m.ops.compactModeGeneration {
			break
		}
		m.ops.compactModeLoading = false
		if msg.Err == nil {
			m.forceCompactMode = msg.Enabled
			m.isCompact = msg.Enabled
			m.updateLayoutAndSize()
			m.dialog.CloseDialog(dialog.CommandsID)
		} else {
			cmds = append(cmds, util.ReportError(msg.Err))
		}

	case notificationStyleSetMsg:
		if msg.generation != m.ops.notificationGeneration {
			break
		}
		m.ops.notificationLoading = false
		if msg.Err != nil {
			cmds = append(cmds, util.ReportError(msg.Err))
			break
		}
		m.updateNotificationBackend()
		m.dialog.CloseDialog(dialog.NotificationsID)
		cmds = append(cmds, util.ReportInfo("Notifications set to: "+msg.Style))

	case permissionResponseMsg:
		if msg.generation != m.ops.permissionGeneration || msg.Permission != m.ops.permissionID {
			break
		}
		m.ops.permissionLoading = false
		if !msg.Accepted {
			cmds = append(cmds, util.ReportError(errors.New("permission response was not accepted")))
			break
		}
		m.dialog.CloseDialog(dialog.PermissionsID)

	case yoloToggledMsg:
		if msg.generation != m.ops.yoloGeneration {
			break
		}
		m.ops.yoloLoading = false
		if msg.Err != nil {
			cmds = append(cmds, util.ReportError(msg.Err))
			break
		}
		m.wsCache.yoloCache.set(msg.Enabled)
		m.wsCache.busyFetchGen++
		m.setEditorPrompt(msg.Enabled)
		status := "disabled"
		if msg.Enabled {
			status = "enabled"
		}
		cmds = append(cmds, util.ReportInfo("Yolo mode "+status))

	case notificationSentMsg:
		m.updateNotificationBackend()

	case importCopilotResult:
		if msg.generation != m.ops.modelOperationGeneration {
			break
		}
		// ImportCopilot completed (successfully or not). Now check
		// whether the provider is actually configured.
		cfg := m.com.Config()
		ws := m.com.Workspace
		isConfigured := func() bool {
			_, ok := cfg.Providers.Get(msg.providerID)
			return ok
		}
		if !isConfigured() {
			m.ops.modelOperationLoading = false
			m.dialog.CloseDialog(dialog.ModelsID)
			provider := catwalk.Provider{ID: catwalk.InferenceProvider(msg.providerID)}
			if cmd := m.openAuthenticationDialog(provider, msg.model); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return cmds, true
		}
		// Provider is configured after import: proceed to UpdatePreferredModel.
		capturedModel := msg.model
		generation := msg.generation
		cmds = append(cmds, func() tea.Msg {
			if err := ws.UpdatePreferredModel(config.ScopeGlobal, capturedModel); err != nil {
				return modelSelectResult{Err: err, generation: generation}
			}
			return modelSelectResult{Onboarding: msg.isOnboarding, Model: capturedModel, generation: generation}
		})
		return cmds, true
	}
	return cmds, false
}
