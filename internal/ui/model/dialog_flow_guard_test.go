package model

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// TestOpenAccountsDialog_LoadsOffThread is the guard's own regression test
// for the offender CLIENT-SERVER.md's PR 0.4 named directly: NewAccounts
// used to call ListAccounts synchronously while building the dialog.
// openAccountsDialog runs on the Update goroutine (it is reached from
// handleKeyPressMsg/applyProviderDialogAction, never from a tea.Cmd), so
// newCmdDrivenUI's guard is on for the whole call — a reintroduced
// synchronous ListAccounts fails this test with "workspace.ListAccounts
// called synchronously on the Update goroutine" before any assertion
// below even runs.
func TestOpenAccountsDialog_LoadsOffThread(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{accs: []accounts.Account{{ID: "acct-1", Label: "Work"}}}
	m := newCmdDrivenUI(t, ws)

	cmd := m.openAccountsDialog(m.com, "test-provider")
	require.NotNil(t, cmd, "opening the dialog must hand back the load as a cmd")
	require.Zero(t, ws.listAccountsCalls, "ListAccounts must not run before the cmd does")

	_, ok := m.dialog.Dialog(dialog.AccountsID).(*dialog.Accounts)
	require.True(t, ok, "expected the accounts dialog to be open in its loading state")

	// cmd batches the dialog's spinner tick alongside the load; run the
	// whole tree (via the guard, so a synchronous ListAccounts anywhere in
	// it still fails loudly) and pick out the ActionAccountsLoaded leaf.
	var loaded dialog.ActionAccountsLoaded
	var found bool
	runCmdTree(m, cmd, func(msg tea.Msg, _ tea.Cmd) {
		if l, ok := msg.(dialog.ActionAccountsLoaded); ok {
			loaded, found = l, true
		}
	})
	require.True(t, found, "expected ActionAccountsLoaded to be delivered")
	require.Equal(t, 1, ws.listAccountsCalls)
	require.Equal(t, ws.accs, loaded.Accounts)
}

// TestOpenDoctorDialog_RunsUnderGuard and TestOpenStatsDialog_RunsUnderGuard
// cover two more of the dialogs listed in CLIENT-SERVER.md's PR 0.4: both
// already deferred their workspace reads into a returned tea.Cmd before
// this change (see dialogs.go's openDoctorDialog/openStatsDialog), so these
// pin that they still do under the guard rather than fixing a regression.
func TestOpenDoctorDialog_RunsUnderGuard(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{}
	m := newCmdDrivenUI(t, ws)

	cmd := m.openDoctorDialog(m.com)
	require.True(t, m.dialog.ContainsDialog(dialog.DoctorID))
	if cmd != nil {
		runGuardedCmd(m, cmd)
	}
}

func TestOpenStatsDialog_RunsUnderGuard(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{}
	m := newCmdDrivenUI(t, ws)

	cmd := m.openStatsDialog()
	require.True(t, m.dialog.ContainsDialog(dialog.StatsID))
	if cmd != nil {
		runGuardedCmd(m, cmd)
	}
}

// openOAuthDialogForKeyTest builds dlg (via a real dialog.NewOAuthX
// constructor, the same one production's configureProvider calls) and
// opens it with m.dialog.OpenDialog rather than production's
// OpenDialogWithGrace — matching command_driving_test.go's own convention
// for driving a key press immediately after opening a dialog (see e.g. its
// permission_flow test): OpenDialogWithGrace absorbs keystrokes for up to
// 425ms specifically so a stray keypress in flight when the dialog appears
// doesn't act on it, which would make a same-tick Enter in this test
// exercise that timer rather than handleProxyKey.
func openOAuthDialogForKeyTest(m *UI, dlg *dialog.OAuth, cmd tea.Cmd) tea.Cmd {
	m.dialog.OpenDialog(dlg)
	return cmd
}

// TestOAuthCodexProxyStep_RejectsInvalidProxy and
// TestOAuthCodexProxyStep_AcceptsValidProxy drive the Codex OAuth dialog's
// proxy step through the real key path (m.Update, not the dialog package's
// own HandleMsg) — this is the regression test for the offender the guard
// missed the first time: handleProxyKey used to call
// configurer.setProxyURL, which called OAuthValidateProxy (class "U",
// wire_classes_test.go) synchronously on the Update goroutine. Enter now
// returns an ActionCmd that validates off-thread; a reintroduced
// synchronous OAuthValidateProxy fails this test with "workspace.
// OAuthValidateProxy called synchronously on the Update goroutine".
func TestOAuthCodexProxyStep_RejectsInvalidProxy(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{oauthValidateProxyErr: errors.New("bad proxy")}
	m := newCmdDrivenUI(t, ws)

	provider := catwalk.Provider{ID: catwalk.InferenceProvider(dialog.CodexProviderID), Name: "OpenAI Codex"}
	dlg, cmd := dialog.NewOAuthCodex(m.com, false, provider, nil, false)
	if cmd := openOAuthDialogForKeyTest(m, dlg, cmd); cmd != nil {
		runCmdTree(m, cmd, nil) // the proxy step's async prefill
	}
	require.Equal(t, dialog.OAuthStateProxy, dlg.State)

	// One step only: the Enter key's own cmd validates off-thread and
	// delivers the rejection back into Update. Its own follow-up cmd is
	// util.ReportError's status toast, whose auto-clear timer is a real
	// multi-second tea.Tick — irrelevant to what this test checks, and
	// this harness runs a tea.Cmd's closure synchronously rather than
	// through the real scheduler, so draining into it would just block.
	_, updateCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	driveCmdStep(m, updateCmd)

	require.Equal(t, dialog.OAuthStateProxy, dlg.State, "an invalid proxy must not start the flow")
	require.Positive(t, ws.oauthValidateProxyCalls)
	require.Zero(t, ws.startOAuthCalls, "StartOAuth must not run for a rejected proxy")
}

func TestOAuthCodexProxyStep_AcceptsValidProxy(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{}
	m := newCmdDrivenUI(t, ws)

	provider := catwalk.Provider{ID: catwalk.InferenceProvider(dialog.CodexProviderID), Name: "OpenAI Codex"}
	dlg, cmd := dialog.NewOAuthCodex(m.com, false, provider, nil, false)
	if cmd := openOAuthDialogForKeyTest(m, dlg, cmd); cmd != nil {
		runCmdTree(m, cmd, nil)
	}
	require.Equal(t, dialog.OAuthStateProxy, dlg.State)

	// Type a proxy value through the real key path, the same way a user
	// would: OAuthStateProxy's default key case updates the field.
	for _, r := range "socks5://x:1" {
		_, keyCmd := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		runCmdTree(m, keyCmd, nil)
	}

	// Enter's own cmd validates off-thread and delivers acceptance, which
	// HandleMsg applies synchronously (moving to Initializing) before
	// handing back the tick+initiateAuth batch as the next cmd.
	_, updateCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	_, batchCmd := driveCmdStep(m, updateCmd)
	require.Equal(t, 1, ws.oauthValidateProxyCalls)
	require.Equal(t, dialog.OAuthStateInitializing, dlg.State, "a valid proxy must start the flow")

	// Run the batch's leaves (spinner tick, initiateAuth) far enough to
	// prove StartOAuth ran off-thread, without following initiateAuth's
	// own result into startPolling's error toast (same reasoning as
	// TestOAuthCodexProxyStep_RejectsInvalidProxy above).
	require.NotNil(t, batchCmd)
	batchMsg := runGuardedCmd(m, batchCmd)
	batch, ok := batchMsg.(tea.BatchMsg)
	require.True(t, ok)
	for _, leaf := range batch {
		if leafMsg := runGuardedCmd(m, leaf); leafMsg != nil {
			m.Update(leafMsg)
		}
	}
	require.Equal(t, 1, ws.startOAuthCalls)
}

// TestOAuthCopilot_RunsUnderGuard drives the Copilot OAuth dialog's
// construction and initiateAuth cmd (see oauth_copilot.go's
// configuredCopilotProxy fix) through the real dialog-open path. It stops
// short of running the "sign-in was not started" error this stub produces
// through to a full Update: that error's util.ReportError schedules the
// status toast's real multi-second auto-clear timer, and running a bare
// tea.Cmd closure (rather than the real Bubble Tea scheduler) blocks on it
// for its whole TTL — unrelated to what this test checks.
func TestOAuthCopilot_RunsUnderGuard(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{oauthConfiguredProxy: "socks5://127.0.0.1:1080"}
	m := newCmdDrivenUI(t, ws)

	provider := catwalk.Provider{ID: catwalk.InferenceProviderCopilot, Name: "GitHub Copilot"}
	dlg, cmd := dialog.NewOAuthCopilot(m.com, false, provider, nil, false)
	m.dialog.OpenDialog(dlg)
	require.Equal(t, dialog.OAuthStateInitializing, dlg.State, "copilot has no proxy step")
	require.NotNil(t, cmd)

	// cmd batches the spinner tick and initiateAuth.
	msg := runGuardedCmd(m, cmd)
	batch, ok := msg.(tea.BatchMsg)
	require.True(t, ok)
	for _, leaf := range batch {
		runGuardedCmd(m, leaf)
	}
	require.Equal(t, 1, ws.startOAuthCalls)
}

// TestMCPAuthDialog_RunsUnderGuard drives the MCP auth dialog's Enter key
// (startAuth) through the real key path. MCPAuthenticate already runs
// off-thread today (authenticateMCP in mcp_auth.go, dispatched from
// updateIntegrations's ActionMCPAuthStarted case), so this pins that
// rather than fixing a regression — but it is exactly the kind of dialog
// the coverage gap in round 1 missed, so it belongs in this file.
func TestMCPAuthDialog_RunsUnderGuard(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{
		mcpPendingAuth: []workspace.MCPPendingAuthServer{{Name: "server1", URL: "http://example.com"}},
	}
	m := newCmdDrivenUI(t, ws)

	cmd := m.openMCPAuthDialog(m.com)
	require.True(t, m.dialog.ContainsDialog(dialog.MCPAuthID))
	if cmd != nil {
		runCmdTree(m, cmd, nil) // the dialog's own opening spinner tick
	}

	_, updateCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, updateCmd, "Submit on the prompt state must start the auth flow")
	runCmdTree(m, updateCmd, nil)

	require.Equal(t, 1, ws.mcpAuthenticateCalls)
	require.Equal(t, "server1", ws.lastMCPAuthName)
}

// TestOpenProviderSettingsDialog_RunsUnderGuard drives the "provider
// settings" dialog's open path: NewProviderSettings reads
// AccountCapabilities (class "C", safe synchronously) then kicks off
// loadAuthStateCmd, which does the workspace read.
func TestOpenProviderSettingsDialog_RunsUnderGuard(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{}
	m := newCmdDrivenUI(t, ws)

	cmd, handled := m.applyProviderDialogAction(dialog.ActionOpenProviderSettings{ProviderID: "test-provider"})
	require.True(t, handled)
	require.True(t, m.dialog.ContainsDialog(dialog.ProviderSettingsID))
	if cmd != nil {
		runCmdTree(m, cmd, nil)
	}
}

// TestSubmitCustomProviderForm_RunsUnderGuard drives ActionSubmitCustomProvider
// the same way dialog_actions_account_edit_test.go drives
// ActionSubmitAccountForm: applying the dialog.Action the form's own Submit
// key produces (that translation is exercised at the dialog package's own
// level), rather than re-typing every field through tea.KeyPressMsg here.
func TestSubmitCustomProviderForm_RunsUnderGuard(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{}
	m := newCmdDrivenUI(t, ws)
	m.openProviderFormDialog(m.com)
	require.True(t, m.dialog.ContainsDialog(dialog.ProviderFormID))

	cmd, handled := m.applyProviderDialogAction(dialog.ActionSubmitCustomProvider{
		ID: "my-custom", BaseURL: "http://localhost:1234", Type: "openai",
	})
	require.True(t, handled)
	require.NotNil(t, cmd, "submitting must hand back a cmd instead of calling the workspace inline")
	require.Zero(t, ws.configureCustomProviderCalls, "ConfigureCustomProvider must not run synchronously")

	msg := runGuardedCmd(m, cmd)
	result, ok := msg.(dialog.ActionCustomProviderResult)
	require.True(t, ok, "expected ActionCustomProviderResult, got %#v", msg)
	require.NoError(t, result.Err)
	require.Equal(t, 1, ws.configureCustomProviderCalls)
}

// TestOpenModelsDialog_RunsUnderGuard opens the models dialog through the
// real path (its constructor prunes stale "recently used" entries via a
// returned tea.Cmd rather than synchronously — see NewFilePicker/NewMCPAuth's
// (*X, tea.Cmd) shape in internal/ui/AGENTS.md).
func TestOpenModelsDialog_RunsUnderGuard(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{}
	m := newCmdDrivenUI(t, ws)

	cmd := m.openModelsDialog(m.com)
	require.True(t, m.dialog.ContainsDialog(dialog.ModelsID))
	if cmd != nil {
		runCmdTree(m, cmd, nil)
	}
}

// TestSelectModel_UpdatesPreferredModelOffThread drives
// dialog.ActionSelectModel — what the Models dialog's Enter/ctrl+e key
// produces (models.go's HandleMsg) — through applyDialogAction, the same
// dispatch point the real key handler feeds it into.
// UpdatePreferredModel must not run before the returned cmd does.
func TestSelectModel_UpdatesPreferredModelOffThread(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{}
	m := newCmdDrivenUI(t, ws)
	warmCmdDrivenCaches(m)

	action := dialog.ActionSelectModel{
		Provider: catwalk.Provider{ID: "test-provider", Name: "Test Provider"},
		Model:    config.SelectedModel{Provider: "test-provider", Model: "m1"},
	}
	cmd := m.applyDialogAction(action)
	require.NotNil(t, cmd)
	require.Zero(t, ws.updatePreferredModelCalls, "UpdatePreferredModel must not run synchronously")

	// One step only: initAgentAndReportModel's own success path reports an
	// info toast whose auto-clear timer is a real multi-second tea.Tick,
	// irrelevant to what this test checks (same reasoning as the OAuth
	// proxy-step tests above) — driving into it would just block on it.
	msg := runGuardedCmd(m, cmd)
	m.Update(msg)
	require.Equal(t, 1, ws.updatePreferredModelCalls)
}

// TestSessionsDialog_RenameAndDelete drives the sessions dialog's open,
// rename (ctrl+r, type, enter) and delete (ctrl+x, y) flows through the
// real key path. openSessionsDialog uses m.dialog.OpenDialog (no grace —
// see applySessionsLoaded), so no grace-period workaround is needed here.
func TestSessionsDialog_RenameAndDelete(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{listSessions: []session.Session{{ID: "s1", Title: "Original"}}}
	m := newCmdDrivenUI(t, ws)
	warmCmdDrivenCaches(m)

	cmd := m.openSessionsDialog()
	require.NotNil(t, cmd)
	runCmdTree(m, cmd, nil)
	require.True(t, m.dialog.ContainsDialog(dialog.SessionsID))

	// Rename: ctrl+r enters rename mode, typing replaces the input, enter
	// confirms and defers RenameSession to a cmd.
	_, rCmd := m.Update(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	runCmdTree(m, rCmd, nil)
	for _, r := range "New" {
		_, keyCmd := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		runCmdTree(m, keyCmd, nil)
	}
	_, confirmCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Zero(t, ws.renameSessionCalls, "RenameSession must not run synchronously")
	runCmdTree(m, confirmCmd, nil)
	require.Equal(t, 1, ws.renameSessionCalls)
	require.Equal(t, "s1", ws.lastRenamedID)

	// Delete: ctrl+x enters delete mode, y confirms and defers
	// DeleteSession to a cmd.
	_, xCmd := m.Update(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
	runCmdTree(m, xCmd, nil)
	_, deleteCmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	require.Zero(t, ws.deleteSessionCalls, "DeleteSession must not run synchronously")
	runCmdTree(m, deleteCmd, nil)
	require.Equal(t, 1, ws.deleteSessionCalls)
	require.Equal(t, "s1", ws.lastDeletedID)
}

// TestCommandPalette_FilterAndRunSummarize opens the command palette,
// types "summarize" to filter to a single item (a command that touches
// the workspace, per the coordinator's ask), and confirms it — pinning
// that AgentSummarize runs off-thread, not inline in HandleMsg/Update.
func TestCommandPalette_FilterAndRunSummarize(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	m := newCmdDrivenUI(t, ws)
	warmCmdDrivenCaches(m)

	cmd := m.openCommandsDialog()
	require.True(t, m.dialog.ContainsDialog(dialog.CommandsID))
	if cmd != nil {
		runCmdTree(m, cmd, nil)
	}

	for _, r := range "summarize" {
		_, keyCmd := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		runCmdTree(m, keyCmd, nil)
	}

	_, confirmCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Zero(t, ws.agentSummarizeCalls, "AgentSummarize must not run synchronously")
	runCmdTree(m, confirmCmd, nil)
	require.Equal(t, 1, ws.agentSummarizeCalls)
}
