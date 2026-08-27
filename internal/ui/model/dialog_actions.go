package model

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
)

// updatePreferredModelCmd returns a tea.Cmd that sets model as the
// workspace's preferred model off the Update goroutine, then hands the
// resulting error (nil on success) to onResult to build whichever message
// the caller's flow continues with. Factors out the "set the model or
// report why not" tea.Cmd shape shared by ActionToggleThinking,
// ActionSelectReasoningEffort, and ActionProviderConfigured below, and by
// importCopilotResult's follow-up in update_settings.go — they differ only
// in what happens next on success (some also call UpdateAgentModel) and in
// which message type carries the result, both of which onResult captures.
func updatePreferredModelCmd(ws workspace.ConfigAccessor, model config.SelectedModel, onResult func(err error) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		return onResult(ws.UpdatePreferredModel(config.ScopeGlobal, model))
	}
}

// applyDialogAction executes a [dialog.Action] regardless of where it came
// from: a dialog's HandleMsg (the usual path, via handleDialogMsg) or a
// command selected directly from the editor's "/" completion popup, which
// has no dialog on the stack at all. CloseDialog/CloseFrontDialog are no-ops
// when their target isn't open, so callers never need to special-case the
// no-dialog path.
func (m *UI) applyDialogAction(action dialog.Action) tea.Cmd {
	if batch, ok := action.(dialog.ActionBatch); ok {
		cmds := make([]tea.Cmd, 0, len(batch.Actions))
		for _, a := range batch.Actions {
			if cmd := m.applyDialogAction(a); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return tea.Batch(cmds...)
	}
	if cmd, handled := m.applySettingsDialogAction(action); handled {
		return cmd
	}
	if cmd, handled := m.applySessionDialogAction(action); handled {
		return cmd
	}
	if cmd, handled := m.applyProviderDialogAction(action); handled {
		return cmd
	}
	return m.applyChromeDialogAction(action)
}

// applySettingsDialogAction handles the command-dialog settings toggles and
// selections: yolo mode, notification style, compact mode, pills, thinking,
// transparent background, theme, reasoning effort, and model selection.
func (m *UI) applySettingsDialogAction(action dialog.Action) (tea.Cmd, bool) {
	var cmds []tea.Cmd

	switch msg := action.(type) {
	// Command dialog messages.
	case dialog.ActionToggleYoloMode:
		cmds = append(cmds, m.toggleYoloMode())
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionSelectNotificationStyle:
		if m.ops.notificationLoading {
			cmds = append(cmds, util.ReportWarn("Notification settings are already being updated"))
			break
		}
		style := msg.Style
		if cfg := m.com.Config(); cfg != nil && cfg.Options != nil {
			m.ops.notificationLoading = true
			m.ops.notificationGeneration++
			generation := m.ops.notificationGeneration
			workspace := m.com.Workspace
			cmds = append(cmds, func() tea.Msg {
				return notificationStyleSetMsg{Err: workspace.SetConfigField(config.ScopeGlobal, "options.notifications", style), Style: style, generation: generation}
			})
		}
	case dialog.ActionToggleCompactMode:
		cmds = append(cmds, m.toggleCompactMode())
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionTogglePills:
		if cmd := m.toggleTodosExpanded(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionToggleThinking:
		if m.ops.modelOperationLoading {
			cmds = append(cmds, util.ReportWarn("Model settings are already being updated"))
			break
		}
		cfg := m.com.Config()
		if cfg == nil {
			cmds = append(cmds, util.ReportError(errors.New("configuration not found")))
			break
		}
		if _, ok := cfg.Agents[config.AgentCoder]; !ok {
			cmds = append(cmds, util.ReportError(errors.New("agent configuration not found")))
			break
		}
		currentModel := cfg.Model
		currentModel.Think = !currentModel.Think
		status := "disabled"
		if currentModel.Think {
			status = "enabled"
		}
		m.ops.modelOperationLoading = true
		m.ops.modelOperationGeneration++
		generation := m.ops.modelOperationGeneration
		ws := m.com.Workspace
		ctx := m.com.Context()
		cmds = append(cmds, updatePreferredModelCmd(ws, currentModel, func(err error) tea.Msg {
			if err != nil {
				return modelSettingUpdatedMsg{Err: err, generation: generation}
			}
			return modelSettingUpdatedMsg{Err: ws.UpdateAgentModel(ctx), Info: "Thinking mode " + status, generation: generation}
		}))
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionToggleTransparentBackground:
		if m.ops.transparentLoading {
			cmds = append(cmds, util.ReportWarn("Transparency is already being updated"))
			break
		}
		cfg := m.com.Config()
		if cfg == nil {
			cmds = append(cmds, util.ReportError(errors.New("configuration not found")))
			break
		}
		desired := cfg.Options == nil || cfg.Options.TUI.Transparent == nil || !*cfg.Options.TUI.Transparent
		m.ops.transparentLoading = true
		m.ops.transparentGeneration++
		generation := m.ops.transparentGeneration
		workspace := m.com.Workspace
		cmds = append(cmds, func() tea.Msg {
			return transparentToggledMsg{Err: workspace.SetConfigField(config.ScopeGlobal, "options.tui.transparent", desired), Enabled: desired, generation: generation}
		})
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionSelectModel:
		if cmd := m.handleSelectModel(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.ActionPreviewTheme:
		if cmd := m.previewTheme(msg.ID); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.ActionSelectTheme:
		if cmd := m.applyTheme(msg.ID); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.dialog.CloseDialog(dialog.ThemeID)
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionSelectReasoningEffort:
		if m.ops.modelOperationLoading {
			cmds = append(cmds, util.ReportWarn("Model settings are already being updated"))
			break
		}
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait..."))
			break
		}

		cfg := m.com.Config()
		if cfg == nil {
			cmds = append(cmds, util.ReportError(errors.New("configuration not found")))
			break
		}

		if _, ok := cfg.Agents[config.AgentCoder]; !ok {
			cmds = append(cmds, util.ReportError(errors.New("agent configuration not found")))
			break
		}

		// The coder agent leaves Model unset (it inherits the app's
		// configured model), so the model it actually runs on is always
		// cfg.Model.
		currentModel := cfg.Model
		currentModel.ReasoningEffort = msg.Effort
		effort := msg.Effort

		m.ops.modelOperationLoading = true
		m.ops.modelOperationGeneration++
		generation := m.ops.modelOperationGeneration
		ws := m.com.Workspace
		ctx := m.com.Context()
		cmds = append(cmds, updatePreferredModelCmd(ws, currentModel, func(err error) tea.Msg {
			if err != nil {
				return modelSettingUpdatedMsg{Err: err, generation: generation}
			}
			return modelSettingUpdatedMsg{Err: ws.UpdateAgentModel(ctx), Info: "Reasoning effort set to " + effort, generation: generation}
		}))
		m.dialog.CloseDialog(dialog.ReasoningID)
	default:
		return nil, false
	}

	return tea.Batch(cmds...), true
}

// applySessionDialogAction handles session-lifecycle dialog messages:
// selecting, creating, and summarizing sessions, permission responses, and
// project initialization.
func (m *UI) applySessionDialogAction(action dialog.Action) (tea.Cmd, bool) {
	var cmds []tea.Cmd

	switch msg := action.(type) {
	// Session dialog messages.
	case dialog.ActionSelectSession:
		m.dialog.CloseDialog(dialog.SessionsID)
		cmds = append(cmds, m.requestSessionLoad(msg.Session.ID))
	case dialog.ActionNewSession:
		var started bool
		if cmds, started = m.startNewSessionGuarded(cmds); started {
			m.dialog.CloseDialog(dialog.CommandsID)
		}
	case dialog.ActionSummarize:
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before summarizing session..."))
			break
		}
		ws := m.com.Workspace
		ctx := m.com.Context()
		sessionID := msg.SessionID
		cmds = append(cmds, func() tea.Msg {
			err := ws.AgentSummarize(ctx, sessionID)
			// A cancellation is the user's own esc, not a failure worth a
			// banner. Summarize reports it as an error deliberately — see
			// its cancel branch, where reporting success instead let the
			// turn continue on the context that needed summarizing.
			if err != nil && !errors.Is(err, context.Canceled) {
				return util.ReportError(err)()
			}
			return nil
		})
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionPermissionResponse:
		if m.ops.permissionLoading {
			cmds = append(cmds, util.ReportWarn("Permission response is already being submitted"))
			break
		}
		m.ops.permissionLoading = true
		m.ops.permissionGeneration++
		generation := m.ops.permissionGeneration
		action := msg.Action
		perm := msg.Permission
		m.ops.permissionID = perm.ID
		permissionID := perm.ID
		workspace := m.com.Workspace
		cmds = append(cmds, func() tea.Msg {
			accepted := false
			switch action {
			case dialog.PermissionAllow:
				accepted = workspace.PermissionGrant(perm)
			case dialog.PermissionAllowForSession:
				accepted = workspace.PermissionGrantPersistent(perm)
			case dialog.PermissionDeny:
				accepted = workspace.PermissionDeny(perm)
			}
			return permissionResponseMsg{Accepted: accepted, Permission: permissionID, generation: generation}
		})
	case dialog.ActionInitializeProject:
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before summarizing session..."))
			break
		}
		cmds = append(cmds, m.initializeProject())
		m.dialog.CloseDialog(dialog.CommandsID)
	default:
		return nil, false
	}

	return tea.Batch(cmds...), true
}

// applyProviderDialogAction handles provider-configuration dialog messages:
// selecting a known provider, opening/submitting the custom-provider form,
// and reacting once a provider has been configured.
func (m *UI) applyProviderDialogAction(action dialog.Action) (tea.Cmd, bool) {
	var cmds []tea.Cmd

	switch msg := action.(type) {
	// Providers configuration dialog messages.
	case dialog.ActionConfigureProvider:
		// A provider that already has credentials offers to switch
		// between its stored accounts instead of starting the auth flow
		// over again. Both checks are pure in-memory reads, so onboarding
		// and a fresh (never-configured) provider are unaffected.
		if pc, ok := m.com.Config().Providers.Get(msg.ProviderID); ok && (pc.APIKey != "" || pc.OAuthToken != nil) {
			if cmd := m.openAccountsDialog(msg.ProviderID); cmd != nil {
				cmds = append(cmds, cmd)
			}
			break
		}
		if cmd := m.configureProvider(msg.ProviderID, false); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.ActionAddAccount:
		// The accounts list was built at load time and won't reflect the
		// new account once sign-in finishes, so rather than leaving it
		// open and stale, close it now: configureProvider opens its own
		// OAuth/API-key dialog on top, and the user is left there instead
		// of back on outdated data. forceNewAccount is true here: the user
		// explicitly chose to add a new account, not (re-)authenticate the
		// one already active.
		m.dialog.CloseDialog(dialog.AccountsID)
		if cmd := m.configureProvider(msg.ProviderID, true); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.ActionOpenCustomProviderForm:
		m.openProviderFormDialog()
	case dialog.ActionOpenAccountEdit:
		m.dialog.OpenDialog(dialog.NewAccountForm(m.com, msg.ProviderID, msg.Account, msg.Active))
	case dialog.ActionSubmitAccountForm:
		ws := m.com.Workspace
		providerID := msg.ProviderID
		account := msg.Account
		cmds = append(cmds, func() tea.Msg {
			err := ws.UpdateAccount(providerID, account)
			return dialog.ActionAccountFormResult{ProviderID: providerID, Err: err}
		})
	case dialog.ActionAccountSaved:
		m.dialog.CloseDialog(dialog.AccountFormID)
		cmds = append(cmds, m.reloadAccountsCmd(msg.ProviderID), m.refreshAccountLabelCmd(msg.ProviderID))
	case dialog.ActionRequestAccountRemoval:
		m.dialog.OpenDialog(dialog.NewAccountRemoveConfirm(m.com, msg.ProviderID, msg.Account))
	case dialog.ActionRemoveAccountConfirmed:
		m.dialog.CloseDialog(dialog.AccountRemoveConfirmID)
		cmds = append(cmds, m.removeAccountCmd(msg.ProviderID, msg.AccountID), m.refreshAccountLabelCmd(msg.ProviderID))
	case dialog.ActionAccountActivated:
		// See ActionAccountActivated's doc comment: this used to be a
		// bare ActionClose{} handled by applyChromeDialogAction's default
		// case below. It is intercepted here instead so a switch to a
		// different account also refreshes the sidebar's cached label
		// for the newly active one (see account_label.go).
		m.dialog.CloseDialog(dialog.AccountsID)
		cmds = append(cmds, m.refreshAccountLabelCmd(msg.ProviderID))
	case dialog.ActionOpenProviderSettings:
		m.dialog.OpenDialog(dialog.NewProviderSettings(m.com, msg.ProviderID))
	case dialog.ActionSubmitProviderSettings:
		ws := m.com.Workspace
		providerID := msg.ProviderID
		proxy := msg.Proxy
		rotation := msg.Rotation
		cmds = append(cmds, func() tea.Msg {
			if err := ws.SetProviderProxy(providerID, proxy); err != nil {
				return dialog.ActionProviderSettingsResult{ProviderID: providerID, Err: err}
			}
			if rotation != nil {
				key := config.ProviderFieldKey(providerID, "rotation")
				if err := ws.SetConfigField(config.ScopeGlobal, key, rotation); err != nil {
					return dialog.ActionProviderSettingsResult{ProviderID: providerID, Err: err}
				}
			}
			return dialog.ActionProviderSettingsResult{ProviderID: providerID}
		})
	case dialog.ActionProviderSettingsSaved:
		m.dialog.CloseDialog(dialog.ProviderSettingsID)
	case dialog.ActionSubmitCustomProvider:
		ws := m.com.Workspace
		ctx := m.com.Context()
		params := workspace.ConfigureCustomProviderParams{
			ID:      msg.ID,
			BaseURL: msg.BaseURL,
			Type:    msg.Type,
			APIKey:  msg.APIKey,
		}
		cmds = append(cmds, func() tea.Msg {
			_, err := workspace.ConfigureCustomProvider(ctx, ws, config.ScopeGlobal, params)
			return dialog.ActionCustomProviderResult{ProviderID: msg.ID, Err: err}
		})
	case dialog.ActionProviderConfigured:
		if m.ops.modelOperationLoading {
			cmds = append(cmds, util.ReportWarn("Model settings are already being updated"))
			break
		}
		m.dialog.CloseDialog(dialog.ProviderFormID)
		m.dialog.CloseDialog(dialog.APIKeyInputID)
		m.dialog.CloseDialog(dialog.OAuthID)
		m.dialog.CloseDialog(dialog.ProvidersID)

		if m.state != uiOnboarding {
			break
		}

		cfg := m.com.Config()
		ws := m.com.Workspace

		model := cfg.Model
		if cfg.GetModel(model.Provider, model.Model) == nil {
			// No valid model carried over (this is the normal first-run
			// case, where no model has ever been selected) — fall back to
			// this provider's own default rather than whatever
			// defaultModelSelection would pick globally.
			knownProviders := config.Providers(cfg)
			def, err := cfg.DefaultModelForProvider(msg.ProviderID, knownProviders)
			if err != nil {
				cmds = append(cmds, util.ReportError(err))
				break
			}
			model = def
		}

		// Move UpdatePreferredModel into a tea.Cmd so it does not block
		// Update.  The result (providerConfiguredResult) is handled in
		// Update and only calls initAgentAndReportModel on success.
		m.ops.modelOperationLoading = true
		m.ops.modelOperationGeneration++
		generation := m.ops.modelOperationGeneration
		capturedModel := model
		cmds = append(cmds, updatePreferredModelCmd(ws, capturedModel, func(err error) tea.Msg {
			if err != nil {
				return providerConfiguredResult{Err: err, generation: generation}
			}
			return providerConfiguredResult{Model: capturedModel, Onboarding: true, generation: generation}
		}))
	default:
		return nil, false
	}

	return tea.Batch(cmds...), true
}

// applyChromeDialogAction handles everything else: generic dialog chrome
// (close/open/cmd), help, external editor, quit, Docker MCP, the threads
// dashboard, file picker selection, custom commands, skills, and MCP
// prompts. It is the last handler in applyDialogAction's chain and, like
// the original switch, falls back to re-dispatching any unrecognized action
// as a plain message.
func (m *UI) applyChromeDialogAction(action dialog.Action) tea.Cmd {
	var cmds []tea.Cmd
	isOnboarding := m.state == uiOnboarding

	switch msg := action.(type) {
	// Generic dialog messages
	case dialog.ActionClose:
		if isOnboarding && m.dialog.ContainsDialog(dialog.ProvidersID) {
			break
		}

		// Leaving the theme picker without choosing puts the palette that
		// was live when it opened back on screen.
		if front := m.dialog.DialogLast(); front != nil && front.ID() == dialog.ThemeID {
			if cmd := m.cancelThemePreview(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}

		m.dialog.CloseFrontDialog()

		if isOnboarding {
			if cmd := m.openProvidersDialog(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}

		if m.focus == uiFocusEditor {
			cmds = append(cmds, m.editor.textarea.Focus())
		}
	case dialog.ActionCmd:
		if msg.Cmd != nil {
			cmds = append(cmds, msg.Cmd)
		}

	// Open dialog message.
	case dialog.ActionOpenDialog:
		m.dialog.CloseDialog(dialog.CommandsID)
		if cmd := m.openDialog(msg.DialogID); cmd != nil {
			cmds = append(cmds, cmd)
		}

	case dialog.ActionToggleHelp:
		m.status.ToggleHelp()
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionExternalEditor:
		var started bool
		if cmds, started = m.openExternalEditorGuarded(cmds); started {
			m.dialog.CloseDialog(dialog.CommandsID)
		}
	case dialog.ActionQuit:
		cmds = append(cmds, tea.Quit)
	case dialog.ActionEnableDockerMCP:
		m.dialog.CloseDialog(dialog.CommandsID)
		cmds = append(cmds, m.enableDockerMCPCmd())
	case dialog.ActionDisableDockerMCP:
		m.dialog.CloseDialog(dialog.CommandsID)
		cmds = append(cmds, m.disableDockerMCPCmd())
	case dialog.ActionOpenThreadsDashboard:
		m.dialog.CloseDialog(dialog.CommandsID)
		cmds = m.openThreadsDashboardGuarded(cmds)

	case dialog.ActionFilePickerSelected:
		m.dialog.CloseDialog(dialog.FilePickerID)
		cmds = append(cmds, msg.Cmd())

	case dialog.ActionRunCustomCommand:
		if len(msg.Arguments) > 0 && msg.Args == nil {
			m.dialog.CloseFrontDialog()
			argsDialog := dialog.NewArguments(
				m.com,
				"Custom Command Arguments",
				"",
				msg.Arguments,
				msg, // Pass the action as the result
			)
			m.dialog.OpenDialog(argsDialog)
			break
		}
		content := msg.Content
		if msg.Args != nil {
			content = substituteArgs(content, msg.Args)
		}
		// If this is a skill command, format it using the skill's FormatInvocation method
		if msg.Skill != nil {
			content = msg.Skill.FormatInvocation()
		}
		cmds = append(cmds, m.sendMessage(content))
		m.dialog.CloseFrontDialog()
	case dialog.ActionAttachSkill:
		m.dialog.CloseFrontDialog()
		cmds = append(cmds, m.attachSkill(msg.ID, msg.Name))

	case dialog.ActionRunMCPPrompt:
		if len(msg.Arguments) > 0 && msg.Args == nil {
			m.dialog.CloseFrontDialog()
			title := cmp.Or(msg.Title, "MCP Prompt Arguments")
			argsDialog := dialog.NewArguments(
				m.com,
				title,
				msg.Description,
				msg.Arguments,
				msg, // Pass the action as the result
			)
			m.dialog.OpenDialog(argsDialog)
			break
		}
		cmds = append(cmds, m.runMCPPrompt(msg.ClientID, msg.PromptID, msg.Args))
	default:
		cmds = append(cmds, util.CmdHandler(msg))
	}

	return tea.Batch(cmds...)
}

// substituteArgs replaces $ARG_NAME placeholders in content with actual
// values.
//
// Longest name first, and that ordering is the point: map iteration is
// random, so with both $FILE and $FILE_PATH defined, whichever came out
// first won — substituting $FILE into "$FILE_PATH" leaves "<value>_PATH",
// and the same command produced different text on different runs. A
// longer name can never be the prefix of a shorter one, so replacing in
// that order is stable and correct.
func substituteArgs(content string, args map[string]string) string {
	names := slices.SortedFunc(maps.Keys(args), func(a, b string) int {
		if d := len(b) - len(a); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	for _, name := range names {
		content = strings.ReplaceAll(content, "$"+name, args[name])
	}
	return content
}
