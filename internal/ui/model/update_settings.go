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

// uiOwned on every settings-result type below: each is dispatched by an
// async config-mutation or model-flow command started from one *UI's own
// dialog (command palette, theme picker, models dialog, permission
// prompt, ...). Routed by active screen instead, the result of a change
// started on one screen applied its generation/loading-state bookkeeping
// to whichever screen was active when it landed, not the one that started
// it — leaving the screen that actually asked stuck "already being
// updated" forever, since only its own result clears that flag.

// transparentToggledMsg carries the result of a transparency-toggle config mutation.
type transparentToggledMsg struct {
	uiOwned

	Err        error
	Enabled    bool
	generation uint64
}

// themeSetMsg carries the result of persisting a theme selection. The
// palette itself is swapped synchronously when the user picks it, so this
// message only reports whether the choice survived to disk; Previous is the
// palette to fall back to if it did not.
type themeSetMsg struct {
	uiOwned

	Err        error
	ID         string
	Previous   string
	generation uint64
}

// compactModeToggledMsg carries the result of a compact-mode config mutation.
type compactModeToggledMsg struct {
	uiOwned

	Err        error
	Enabled    bool
	generation uint64
}

// providerConfiguredResult carries the outcome of the async provider-config
// flow (UpdatePreferredModel + init) dispatched by ActionProviderConfigured.
type providerConfiguredResult struct {
	uiOwned

	Err        error
	Model      config.SelectedModel
	Onboarding bool
	generation uint64
}

// modelSelectResult carries the outcome of the async model-select flow
// dispatched by handleSelectModel.
type modelSelectResult struct {
	uiOwned

	Err        error
	Onboarding bool
	Model      config.SelectedModel
	generation uint64
}

type agentModelInitializedMsg struct {
	uiOwned

	Err        error
	Onboarding bool
	Model      config.SelectedModel
	generation uint64
}

type modelSettingUpdatedMsg struct {
	uiOwned

	Err        error
	Info       string
	generation uint64
}

// notificationStyleSetMsg carries the result of a notification-style config mutation.
type notificationStyleSetMsg struct {
	uiOwned

	Err        error
	Style      string
	generation uint64
}

type yoloToggledMsg struct {
	uiOwned

	Err        error
	Enabled    bool
	generation uint64
}

// yoloPermissionEnabledMsg carries the result of enabling yolo mode and
// granting the permission that prompted the choice. These are two
// separate workspace calls (PermissionSetSkipRequests then, only if that
// succeeded, PermissionGrant), and a single Err field would conflate two
// very different outcomes: when SkipErr is set, skip-requests itself
// failed, so yolo was never turned on and stays reported off. When
// GrantErr is set instead, skip-requests already succeeded before the
// grant was attempted — the workspace is auto-approving everything
// regardless of what the grant call did next, so yolo must be reported
// enabled (cache/prompt updated) even though the permission dialog stays
// open for a retry of the grant. Accepted is only meaningful when GrantErr
// is nil; it means what permissionResponseMsg's Accepted means.
type yoloPermissionEnabledMsg struct {
	uiOwned

	Accepted             bool
	SkipErr              error
	GrantErr             error
	Permission           string
	permissionGeneration uint64
	yoloGeneration       uint64
}

// permissionResponseMsg carries the result of resolving a permission
// request. See yoloPermissionEnabledMsg's Err for what distinguishes it
// from Accepted=false.
type permissionResponseMsg struct {
	uiOwned

	Accepted   bool
	Err        error
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
		if !m.modelOperation.owns(msg.generation) {
			break
		}
		if msg.Err != nil {
			m.modelOperation.complete(msg.generation)
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
		cmds = append(cmds, refreshAccountLabelCmd(m.com, m, msg.Model.Provider))

	case modelSelectResult:
		if !m.modelOperation.owns(msg.generation) {
			break
		}
		if msg.Err != nil {
			m.modelOperation.complete(msg.generation)
			cmds = append(cmds, util.ReportError(msg.Err))
			break
		}
		cmds = append(cmds, m.initAgentAndReportModel(msg.Onboarding, msg.Model, msg.generation))
		// Switching models can switch providers, and the sidebar's
		// label cache is per provider: without this, moving to a
		// provider the UI had not seen at startup would render its plan
		// line with no account label until something else happened to
		// refresh it (see account_label.go).
		cmds = append(cmds, refreshAccountLabelCmd(m.com, m, msg.Model.Provider))

	case agentModelInitializedMsg:
		if !m.modelOperation.owns(msg.generation) {
			break
		}
		if !m.modelOperation.complete(msg.generation) {
			break
		}
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
		cmds = append(cmds, util.ReportInfo(fmt.Sprintf("Model changed to %s", modelName)), func() tea.Msg { return agentModelChangedCmd(m) })

	case modelSettingUpdatedMsg:
		if !m.modelOperation.owns(msg.generation) {
			break
		}
		if !m.modelOperation.complete(msg.generation) {
			break
		}
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
		if !m.themePersistence.complete(msg.generation) {
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

	case yoloPermissionEnabledMsg:
		yoloCompleted := m.yolo.complete(msg.yoloGeneration)
		permissionCompleted := m.permissionResponse.complete(msg.Permission, msg.permissionGeneration)
		if yoloCompleted {
			if msg.SkipErr != nil {
				// PermissionSetSkipRequests itself failed: skip-requests
				// never took effect in the workspace, so yolo stays
				// reported off.
				cmds = append(cmds, util.ReportError(fmt.Errorf("enabling yolo mode: %w", msg.SkipErr)))
			} else {
				// Skip-requests succeeded, whatever GrantErr says below:
				// the workspace is now auto-approving every request, so
				// the UI must reflect that regardless of whether the
				// grant that prompted this also succeeded.
				m.wsCache.yoloCache.Set(true)
				m.wsCache.busyFetchGen++
				m.setEditorPrompt(true)
				cmds = append(cmds, util.ReportInfo("Yolo mode enabled"))
			}
		}
		if permissionCompleted {
			if msg.SkipErr != nil {
				// Skip-requests failed, so PermissionGrant was never
				// attempted (see the dispatching Cmd) — this is the same
				// call-could-not-be-carried-out case as GrantErr, just
				// caught one call earlier. Leave the dialog open for a
				// retry. The yolo branch above already reported it when
				// it owned this generation.
				if !yoloCompleted {
					cmds = append(cmds, util.ReportError(msg.SkipErr))
				}
				break
			}
			if msg.GrantErr != nil {
				// The grant call itself failed; leave the dialog open so
				// the user can retry it, same as permissionResponseMsg's
				// Err handling.
				cmds = append(cmds, util.ReportError(msg.GrantErr))
				break
			}
			m.dialog.CloseDialog(dialog.PermissionsID)
			if !msg.Accepted {
				cmds = append(cmds, util.ReportError(errors.New("permission request is no longer waiting for an answer")))
			}
		}

	case permissionResponseMsg:
		if !m.permissionResponse.complete(msg.Permission, msg.generation) {
			break
		}
		if msg.Err != nil {
			// The call could not be carried out at all; keep the dialog
			// open (permissionResponse.complete already cleared the
			// in-flight state above, so a retry is allowed) rather than
			// treating this like a lost race.
			cmds = append(cmds, util.ReportError(msg.Err))
			break
		}
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
		if !m.yolo.complete(msg.generation) {
			break
		}
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
		if !m.modelOperation.owns(msg.generation) {
			break
		}
		if msg.Err != nil {
			cmds = append(cmds, util.ReportError(fmt.Errorf("importing github copilot token: %w", msg.Err)))
		}
		// ImportCopilot completed (successfully or not). Now check
		// whether the provider is actually configured.
		cfg := m.com.Config()
		ws := m.com.Workspace
		isConfigured := func() bool {
			_, ok := cfg.Provider(msg.providerID)
			return ok
		}
		if !isConfigured() {
			m.modelOperation.complete(msg.generation)
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
				return modelSelectResult{uiOwned: uiOwned{owner: m}, Err: err, generation: generation}
			}
			return modelSelectResult{uiOwned: uiOwned{owner: m}, Onboarding: msg.isOnboarding, Model: capturedModel, generation: generation}
		}))
		return cmds, true
	}
	return cmds, false
}
