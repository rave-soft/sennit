package model

import (
	"fmt"
	"slices"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/event"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/ui/completions"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/util"
)

// closeDialogMsg is sent to close the current dialog.
type closeDialogMsg struct{}

func (m *UI) handleDialogMsg(msg tea.Msg) tea.Cmd {
	action := m.dialog.Update(msg)
	if action == nil {
		return tea.Batch()
	}
	return m.applyDialogAction(action)
}

func (m *UI) openAuthenticationDialog(provider catwalk.Provider, model config.SelectedModel) tea.Cmd {
	var (
		dlg dialog.Dialog
		cmd tea.Cmd

		isOnboarding = m.state == uiOnboarding
	)

	switch provider.ID {
	case catwalk.InferenceProviderCopilot:
		dlg, cmd = dialog.NewOAuthCopilot(m.com, isOnboarding, provider, &model)
	case catwalk.InferenceProvider(codex.ProviderID):
		dlg, cmd = dialog.NewOAuthCodex(m.com, isOnboarding, provider, &model)
	default:
		dlg, cmd = dialog.NewAPIKeyInput(m.com, isOnboarding, provider, &model)
	}

	if m.dialog.ContainsDialog(dlg.ID()) {
		m.dialog.BringToFront(dlg.ID())
		return nil
	}

	m.dialog.OpenDialogWithGrace(dlg)
	return cmd
}

// openDialog opens a dialog by its ID.
func (m *UI) openDialog(id string) tea.Cmd {
	var cmds []tea.Cmd
	switch id {
	case dialog.SessionsID:
		if cmd := m.openSessionsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.ModelsID:
		if cmd := m.openModelsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.CommandsID:
		if cmd := m.openCommandsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.ReasoningID:
		if cmd := m.openReasoningDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.ThemeID:
		if cmd := m.openThemeDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.ProvidersID:
		if cmd := m.openProvidersDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.NotificationsID:
		if cmd := m.openNotificationsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.FilePickerID:
		if cmd := m.openFilesDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.QuitID:
		m.openQuitDialog()
	case dialog.DoctorID:
		m.openDoctorDialog()
	case dialog.StatsID:
		if cmd := m.openStatsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	default:
		// Unknown dialog
		break
	}
	return tea.Batch(cmds...)
}

// openQuitDialog opens the quit confirmation dialog.
func (m *UI) openQuitDialog() {
	if m.dialog.ContainsDialog(dialog.QuitID) {
		// Bring to front
		m.dialog.BringToFront(dialog.QuitID)
		return
	}

	quitDialog := dialog.NewQuit(m.com)
	m.dialog.OpenDialog(quitDialog)
}

// openModelsDialog opens the models dialog.
func (m *UI) openModelsDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.ModelsID) {
		// Bring to front
		m.dialog.BringToFront(dialog.ModelsID)
		return nil
	}

	modelsDialog, pruneCmd, err := dialog.NewModels(m.com)
	if err != nil {
		return util.ReportError(err)
	}

	m.dialog.OpenDialog(modelsDialog)

	return pruneCmd
}

// openCommandsDialog opens the commands palette dialog (ctrl+p): the
// mouse-friendly fallback for browsing commands. The editor's "/" trigger no
// longer opens this — see commandCompletionItems for that path — but both
// draw from the same command list via dialog.BuildCommandItems.
func (m *UI) openCommandsDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.CommandsID) {
		// Bring to front
		m.dialog.BringToFront(dialog.CommandsID)
		return nil
	}

	var sessionID string
	hasSession := m.sess.current != nil
	if hasSession {
		sessionID = m.sess.current.ID
	}
	hasTodos := hasSession && hasIncompleteTodos(m.sess.current.Todos)
	hasQueue := len(m.wsCache.promptQueueCache.value) > 0

	commands, err := dialog.NewCommands(m.com, sessionID, hasSession, hasTodos, hasQueue, m.customCommands, m.mcpPrompts)
	if err != nil {
		return util.ReportError(err)
	}

	m.dialog.OpenDialog(commands)

	return commands.InitialCmd()
}

// commandCompletionItems builds the flat command list for the editor's "/"
// completion popup, from the same dialog.BuildCommandItems provider the
// Commands palette dialog uses, so the two never list different commands.
func (m *UI) commandCompletionItems() []completions.CommandCompletionValue {
	var sessionID string
	hasSession := m.sess.current != nil
	if hasSession {
		sessionID = m.sess.current.ID
	}
	hasTodos := hasSession && hasIncompleteTodos(m.sess.current.Todos)
	hasQueue := len(m.wsCache.promptQueueCache.value) > 0

	var dockerMCPAvailable *bool
	if available, known := config.DockerMCPAvailabilityCached(); known {
		dockerMCPAvailable = &available
	}

	items := dialog.BuildCommandItems(m.com, sessionID, hasSession, hasTodos, hasQueue, m.lay.width, dockerMCPAvailable, m.customCommands, m.mcpPrompts)
	values := make([]completions.CommandCompletionValue, 0, len(items))
	for _, item := range items {
		values = append(values, completions.CommandCompletionValue{
			ID:          item.ID(),
			Title:       item.Title(),
			Aliases:     item.Aliases(),
			Description: item.Description(),
			Shortcut:    item.Shortcut(),
			Action:      item.Action(),
		})
	}
	return values
}

// openReasoningDialog opens the reasoning effort dialog.
func (m *UI) openReasoningDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.ReasoningID) {
		m.dialog.BringToFront(dialog.ReasoningID)
		return nil
	}

	reasoningDialog, err := dialog.NewReasoning(m.com)
	if err != nil {
		return util.ReportError(err)
	}

	m.dialog.OpenDialog(reasoningDialog)
	return nil
}

// openThemeDialog opens the color theme picker — see the "select_theme"
// entry in dialog/commands.go ("/theme").
func (m *UI) openThemeDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.ThemeID) {
		m.dialog.BringToFront(dialog.ThemeID)
		return nil
	}

	themeDialog, err := dialog.NewTheme(m.com)
	if err != nil {
		return util.ReportError(err)
	}

	m.dialog.OpenDialog(themeDialog)
	return nil
}

// openProvidersDialog opens the providers configuration dialog. It's the
// onboarding entry point (see Init) as well as reachable from the command
// palette — see the "configure_providers" entry in dialog/commands.go.
func (m *UI) openProvidersDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.ProvidersID) {
		m.dialog.BringToFront(dialog.ProvidersID)
		return nil
	}

	isOnboarding := m.state == uiOnboarding
	providersDialog, err := dialog.NewProviders(m.com, isOnboarding)
	if err != nil {
		return util.ReportError(err)
	}

	m.dialog.OpenDialog(providersDialog)
	return nil
}

// openAccountsDialog opens the accounts dialog for providerID, letting the
// user switch between stored credentialed accounts.
func (m *UI) openAccountsDialog(providerID string) tea.Cmd {
	if m.dialog.ContainsDialog(dialog.AccountsID) {
		m.dialog.BringToFront(dialog.AccountsID)
		return nil
	}

	accountsDialog, cmd := dialog.NewAccounts(m.com, providerID)
	m.dialog.OpenDialog(accountsDialog)
	return cmd
}

// openProviderFormDialog opens the custom provider form dialog.
func (m *UI) openProviderFormDialog() {
	if m.dialog.ContainsDialog(dialog.ProviderFormID) {
		m.dialog.BringToFront(dialog.ProviderFormID)
		return
	}

	formDialog := dialog.NewProviderForm(m.com)
	m.dialog.OpenDialog(formDialog)
}

// configureProvider resolves providerID to its catalog entry and opens the
// matching authentication dialog (OAuth for Copilot, API key input
// otherwise), mirroring openAuthenticationDialog's dispatch. The API key
// dialog is opened in its model-less mode (nil model) since this flow
// isn't switching a model, just authenticating a provider.
func (m *UI) configureProvider(providerID string) tea.Cmd {
	providers := config.Providers(m.com.Config())

	idx := slices.IndexFunc(providers, func(p catwalk.Provider) bool {
		return string(p.ID) == providerID
	})
	if idx == -1 {
		return util.ReportError(fmt.Errorf("unknown provider %q", providerID))
	}
	provider := providers[idx]

	var (
		dlg dialog.Dialog
		cmd tea.Cmd

		isOnboarding = m.state == uiOnboarding
	)
	switch provider.ID {
	case catwalk.InferenceProviderCopilot:
		dlg, cmd = dialog.NewOAuthCopilot(m.com, isOnboarding, provider, nil)
	case catwalk.InferenceProvider(codex.ProviderID):
		dlg, cmd = dialog.NewOAuthCodex(m.com, isOnboarding, provider, nil)
	default:
		dlg, cmd = dialog.NewAPIKeyInput(m.com, isOnboarding, provider, nil)
	}

	if m.dialog.ContainsDialog(dlg.ID()) {
		m.dialog.BringToFront(dlg.ID())
		return nil
	}

	m.dialog.OpenDialogWithGrace(dlg)
	return cmd
}

// openNotificationsDialog opens the notification style picker dialog.
//
//nolint:unparam // always nil today, but matches the tea.Cmd signature shared by the other open*Dialog methods
func (m *UI) openNotificationsDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.NotificationsID) {
		m.dialog.BringToFront(dialog.NotificationsID)
		return nil
	}

	notificationsDialog := dialog.NewNotifications(m.com)
	m.dialog.OpenDialog(notificationsDialog)
	return nil
}

// openStatsDialog opens the /stats usage screen and kicks off the first
// tab's aggregation. The gather runs as a command rather than inline
// because the global scope sweeps the whole message history, which is not
// work the render loop should be holding.
func (m *UI) openStatsDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.StatsID) {
		m.dialog.BringToFront(dialog.StatsID)
		return nil
	}

	event.StatsViewed()
	// No session yet is an ordinary state — the screen opens on the
	// project tab and its Session tab says there is nothing to report.
	// m.sess.current is a pointer that is nil until one is loaded.
	var sessionID string
	if m.sess.current != nil {
		sessionID = m.sess.current.ID
	}
	statsDialog := dialog.NewStats(m.com, sessionID)
	m.dialog.OpenDialog(statsDialog)
	return statsDialog.LoadCmd()
}

// openDoctorDialog opens the /doctor config-problems dialog.
func (m *UI) openDoctorDialog() {
	if m.dialog.ContainsDialog(dialog.DoctorID) {
		m.dialog.BringToFront(dialog.DoctorID)
		return
	}

	m.dialog.OpenDialog(dialog.NewDoctor(m.com))
}

// openSessionsDialog opens the sessions dialog. If the dialog is already
// open, it brings it to the front. Otherwise it dispatches an off-thread
// ListSessions fetch (treated as IO — see workspace_cache.go) and opens
// the dialog once sessionsLoadedMsg lands; see applySessionsLoaded.
func (m *UI) openSessionsDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.SessionsID) {
		// Bring to front
		m.dialog.BringToFront(dialog.SessionsID)
		return nil
	}
	if m.sess.dialogLoading {
		// A fetch is already in flight; don't stack another one.
		return nil
	}

	selectedSessionID := ""
	if m.sess.current != nil {
		selectedSessionID = m.sess.current.ID
	}

	m.sess.dialogLoading = true
	m.sess.dialogGen++
	gen := m.sess.dialogGen
	ws := m.com.Workspace
	ctx := m.com.Context()
	owner := m
	return func() tea.Msg {
		sessions, err := ws.ListSessions(ctx)
		return sessionsLoadedMsg{
			uiOwned:           uiOwned{owner: owner},
			gen:               gen,
			sessions:          sessions,
			selectedSessionID: selectedSessionID,
			err:               err,
		}
	}
}

// openFilesDialog opens the file picker dialog.
func (m *UI) openFilesDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.FilePickerID) {
		// Bring to front
		m.dialog.BringToFront(dialog.FilePickerID)
		return nil
	}

	filePicker, cmd := dialog.NewFilePicker(m.com)
	filePicker.SetImageCapabilities(&m.caps)
	m.dialog.OpenDialog(filePicker)
	size := tea.WindowSizeMsg{Width: m.caps.Columns, Height: m.caps.Rows}
	if layoutCmd := m.applyDialogAction(m.dialog.UpdateDialog(dialog.FilePickerID, dialog.FilePickerUpdateMsg{Capabilities: m.caps, WindowSize: &size})); layoutCmd != nil {
		cmd = tea.Batch(cmd, layoutCmd)
	}
	event.FilePickerOpened()

	return cmd
}

// openPermissionsDialog opens the permissions dialog for a permission request.
//
//nolint:unparam // always nil today, but matches the tea.Cmd signature shared by the other open*Dialog methods
func (m *UI) openPermissionsDialog(perm permission.PermissionRequest) tea.Cmd {
	// One request can reach this UI twice. While drilled into a thread,
	// its permission traffic arrives both through the thread's own event
	// pump (Root.handleThreadAttached's SubscribeWith) and through the
	// relay into the parent's stream (thread lifecycle.forwardPermissions),
	// and the router hands both to the thread's embedded UI.
	//
	// Reopening for the second copy is not a cosmetic double-render: it
	// bumps the generation that an answer already in flight is matched
	// against, so that answer is dropped on arrival, and it leaves a fresh
	// dialog standing for a request that has already been decided.
	// Granting an already-decided request is refused, so that dialog can
	// never be dismissed — and on the thread screen the refusal is not
	// even visible, since errors there have nowhere to render yet (see
	// Root.Update's util.InfoMsg case). The result is a permission prompt
	// the user cannot answer or escape.
	//
	// Keyed on the request id, so a genuinely new request still replaces
	// whatever is open.
	if m.ops.permissionID == perm.ID && m.dialog.Dialog(dialog.PermissionsID) != nil {
		return nil
	}
	m.ops.permissionGeneration++
	m.ops.permissionLoading = false
	m.ops.permissionID = perm.ID
	// Close any existing permissions dialog first.
	m.dialog.CloseDialog(dialog.PermissionsID)

	// Get diff mode from config.
	var opts []dialog.PermissionsOption
	if diffMode := m.com.Config().Options.TUI.DiffMode; diffMode != "" {
		opts = append(opts, dialog.WithDiffMode(diffMode == "split"))
	}

	permDialog := dialog.NewPermissions(m.com, perm, opts...)
	m.dialog.OpenDialogWithGrace(permDialog)
	return nil
}

// openBatchFormDialog activates a tabbed multi-question form in
// the editor area. Single questions render without tabs or confirm.
func (m *UI) openBatchFormDialog(batch question.Request) {
	// Close any existing question form first to prevent stacking.
	if qf, ok := m.activeInline.(*dialog.QuestionForm); ok && qf != nil {
		m.activeInline = nil
	}

	form := dialog.NewQuestionForm(m.com.Styles, batch)
	// QuestionAnswer/QuestionCancel resolve the question service's
	// pending channel; wiring them as commands (not direct calls here)
	// keeps that off the Update goroutine, matching the rest of this
	// dialog's IO. Snapshot the workspace by value so the closures don't
	// race with Update reading m.com off the render loop.
	ws := m.com.Workspace
	form.OnAnswer = func(responses []question.Answer) tea.Cmd {
		return func() tea.Msg {
			ws.QuestionAnswer(responses)
			return nil
		}
	}
	form.OnCancel = func() tea.Cmd {
		return func() tea.Msg {
			ws.QuestionCancel()
			return nil
		}
	}
	m.activeInline = form
	m.editor.textarea.Blur()
	m.focus = uiFocusEditor
	m.activeInline.SetFocused(true)
	m.updateLayoutAndSize()
}

// shouldCollapseQuestion reports whether a question form should render
// in its collapsed one-line view. This is true only when the form is
// unfocused and would consume more than half the terminal height.
func (m *UI) shouldCollapseQuestion(qf *dialog.QuestionForm) bool {
	return m.focus != uiFocusEditor && m.lay.height > 0 && qf.Height(m.editorContentWidth()) > m.lay.height*2/5
}
