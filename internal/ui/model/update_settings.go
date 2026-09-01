package model

import (
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/ui/util"
)

// settingsOps holds the generation counters and in-flight flags for the
// settings that apply asynchronously (yolo, model, theme, permission
// responses). A generation is
// bumped each time an operation starts, and the result handler discards
// any reply whose generation doesn't match the latest one, so a stale
// response from a superseded operation can't clobber a newer one.
type settingsOps struct {
	yoloGeneration           uint64
	modelOperationGeneration uint64
	modelOperationLoading    bool
	themeGeneration          uint64
	yoloLoading              bool
	permissionLoading        bool
	permissionGeneration     uint64
	permissionID             string
}

// transparentToggledMsg carries the result of a transparency-toggle config mutation.
type transparentToggledMsg struct {
	Err        error
	Enabled    bool
	generation uint64
}

// themeSetMsg carries the result of persisting a theme selection. The
// palette itself is swapped synchronously when the user picks it, so this
// message only reports whether the choice survived to disk; Previous is the
// palette to fall back to if it did not.
type themeSetMsg struct {
	Err        error
	ID         string
	Previous   string
	generation uint64
}

// compactModeToggledMsg carries the result of a compact-mode config mutation.
type compactModeToggledMsg struct {
	Err        error
	Enabled    bool
	generation uint64
}

// providerConfiguredResult carries the outcome of the async provider-config
// flow (UpdatePreferredModel + init) dispatched by ActionProviderConfigured.
type providerConfiguredResult struct {
	Err        error
	Model      config.SelectedModel
	Onboarding bool
	generation uint64
}

// modelSelectResult carries the outcome of the async model-select flow
// dispatched by handleSelectModel.
type modelSelectResult struct {
	Err        error
	Onboarding bool
	Model      config.SelectedModel
	generation uint64
}

type agentModelInitializedMsg struct {
	Err        error
	Onboarding bool
	Model      config.SelectedModel
	generation uint64
}

type modelSettingUpdatedMsg struct {
	Err        error
	Info       string
	generation uint64
}

// notificationStyleSetMsg carries the result of a notification-style config mutation.
type notificationStyleSetMsg struct {
	Err        error
	Style      string
	generation uint64
}

type yoloToggledMsg struct {
	Err        error
	Enabled    bool
	generation uint64
}

type permissionResponseMsg struct {
	Accepted   bool
	Permission string
	generation uint64
}

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
		// A provider configured through the accounts dialog's "Add
		// account…" ends up here too, once sign-in finishes — refresh
		// the sidebar's cached account label alongside the model/agent
		// init above (see account_label.go). Harmless for every other
		// caller of this same success path: refreshAccountLabelCmd is a
		// cheap no-op for a single-account provider.
		cmds = append(cmds, refreshAccountLabelCmd(m.com, msg.Model.Provider))

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
		// Switching models can switch providers, and the sidebar's
		// label cache is per provider: without this, moving to a
		// provider the UI had not seen at startup would render its plan
		// line with no account label until something else happened to
		// refresh it (see account_label.go).
		cmds = append(cmds, refreshAccountLabelCmd(m.com, msg.Model.Provider))

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
		if !m.transparency.complete(msg.generation) {
			break
		}
		if msg.Err != nil {
			cmds = append(cmds, util.ReportError(msg.Err))
			break
		}
		m.lay.isTransparent = msg.Enabled
		m.dialog.CloseDialog(dialog.CommandsID)

	case themeSetMsg:
		if msg.generation != m.ops.themeGeneration {
			break
		}
		if msg.Err != nil {
			// The palette was swapped optimistically; put it back so what
			// is on screen matches what is on disk.
			if cmd := m.setTheme(msg.Previous); cmd != nil {
				cmds = append(cmds, cmd)
			}
			cmds = append(cmds, util.ReportError(msg.Err))
			break
		}
		cmds = append(cmds, util.ReportInfo("Theme set to: "+styles.PaletteByID(msg.ID).Name))

	case compactModeToggledMsg:
		if !m.compactMode.complete(msg.generation) {
			break
		}
		if msg.Err == nil {
			m.lay.forceCompactMode = msg.Enabled
			m.lay.isCompact = msg.Enabled
			m.updateLayoutAndSize()
			m.dialog.CloseDialog(dialog.CommandsID)
		} else {
			cmds = append(cmds, util.ReportError(msg.Err))
		}

	case notificationStyleSetMsg:
		if !m.notificationStyle.complete(msg.generation) {
			break
		}
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
			// Nothing is holding this request any more: an answer is
			// refused only when no permission service still has the id
			// pending (see permission.resolve), which means it was
			// already decided, or the run that raised it ended. Close
			// the dialog anyway. Leaving it up was the worse half of
			// this failure -- the prompt could not be answered and could
			// not be dismissed either, so the session was stuck behind a
			// dead modal.
			m.dialog.CloseDialog(dialog.PermissionsID)
			cmds = append(cmds, util.ReportError(errors.New("permission request is no longer waiting for an answer")))
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
		m.wsCache.yoloCache.Set(msg.Enabled)
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
		cmds = append(cmds, updatePreferredModelCmd(ws, capturedModel, func(err error) tea.Msg {
			if err != nil {
				return modelSelectResult{Err: err, generation: generation}
			}
			return modelSelectResult{Onboarding: msg.isOnboarding, Model: capturedModel, generation: generation}
		}))
		return cmds, true
	}
	return cmds, false
}
