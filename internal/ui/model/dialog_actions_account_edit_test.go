package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// findAccountsMsg runs cmd (unwrapping a tea.BatchMsg if that's what it
// produces) and returns the first message matching match.
func findAccountsMsg(t *testing.T, cmd tea.Cmd, match func(tea.Msg) bool) tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if match(msg) {
		return msg
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if found := findAccountsMsg(t, c, match); found != nil {
				return found
			}
		}
	}
	return nil
}

// TestApplyProviderDialogAction_OpenAccountEdit_OpensForm covers "e" in the
// accounts dialog: ActionOpenAccountEdit must open AccountForm pre-filled
// with the selected account and its active flag, with no IO along the way.
func TestApplyProviderDialogAction_OpenAccountEdit_OpensForm(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{}
	m := newCmdDrivenUI(ws)

	account := accounts.Account{ID: "acct-1", Label: "Work"}
	_, handled := m.applyProviderDialogAction(dialog.ActionOpenAccountEdit{
		ProviderID: "test-provider", Account: account, Active: true,
	})
	require.True(t, handled)
	require.Zero(t, ws.updateAccountCalls, "opening the form must not touch the workspace")

	_, ok := m.dialog.Dialog(dialog.AccountFormID).(*dialog.AccountForm)
	require.True(t, ok, "expected the account form to be open")
}

// TestApplyProviderDialogAction_SubmitAccountForm_CallsUpdateAccountOffThread
// mirrors the ProviderForm/ActionSubmitCustomProvider wiring: submitting the
// form must not call UpdateAccount synchronously — only the returned
// tea.Cmd does, and its result is addressed back to the form.
func TestApplyProviderDialogAction_SubmitAccountForm_CallsUpdateAccountOffThread(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{}
	m := newCmdDrivenUI(ws)

	account := accounts.Account{ID: "acct-1", Label: "Renamed", ProxyURL: "http://proxy:8080"}
	cmd, handled := m.applyProviderDialogAction(dialog.ActionSubmitAccountForm{
		ProviderID: "test-provider", Account: account,
	})
	require.True(t, handled)
	require.NotNil(t, cmd)
	require.Zero(t, ws.updateAccountCalls, "UpdateAccount must not run synchronously")

	msg := findAccountsMsg(t, cmd, func(msg tea.Msg) bool {
		_, ok := msg.(dialog.ActionAccountFormResult)
		return ok
	})
	result, ok := msg.(dialog.ActionAccountFormResult)
	require.True(t, ok, "expected ActionAccountFormResult, got %#v", msg)
	require.NoError(t, result.Err)
	require.Equal(t, 1, ws.updateAccountCalls)
	require.Equal(t, account, ws.lastUpdatedAccount)
}

// TestApplyProviderDialogAction_AccountSaved_ClosesFormAndReloadsList
// covers what happens once the form's save succeeds: the form closes and
// the accounts list is reloaded (ListAccounts called again) rather than
// left showing stale data.
func TestApplyProviderDialogAction_AccountSaved_ClosesFormAndReloadsList(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{accs: []accounts.Account{{ID: "acct-1", Label: "Renamed"}}}
	m := newCmdDrivenUI(ws)
	m.dialog.OpenDialog(dialog.NewAccountForm(m.com, "test-provider", accounts.Account{ID: "acct-1"}, false))

	cmd, handled := m.applyProviderDialogAction(dialog.ActionAccountSaved{ProviderID: "test-provider"})
	require.True(t, handled)
	require.False(t, m.dialog.ContainsDialog(dialog.AccountFormID), "the form must close on save")
	require.NotNil(t, cmd)

	msg := findAccountsMsg(t, cmd, func(msg tea.Msg) bool {
		_, ok := msg.(dialog.ActionAccountsLoaded)
		return ok
	})
	loaded, ok := msg.(dialog.ActionAccountsLoaded)
	require.True(t, ok, "expected ActionAccountsLoaded, got %#v", msg)
	require.Equal(t, 1, ws.listAccountsCalls)
	require.Equal(t, ws.accs, loaded.Accounts)
}

// TestApplyProviderDialogAction_RequestAccountRemoval_OpensConfirm covers
// "d" in the accounts dialog: it must open the confirmation dialog rather
// than remove anything itself.
func TestApplyProviderDialogAction_RequestAccountRemoval_OpensConfirm(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{}
	m := newCmdDrivenUI(ws)

	account := accounts.Account{ID: "acct-1", Label: "Work"}
	_, handled := m.applyProviderDialogAction(dialog.ActionRequestAccountRemoval{
		ProviderID: "test-provider", Account: account,
	})
	require.True(t, handled)
	require.Zero(t, ws.removeAccountCalls, "requesting removal must not remove anything without confirmation")
	require.True(t, m.dialog.ContainsDialog(dialog.AccountRemoveConfirmID))
}

// TestApplyProviderDialogAction_RemoveAccountConfirmed_RemovesAndReloadsList
// covers the confirmed path: RemoveAccount runs, and on success the
// accounts list is reloaded the same way a successful edit reloads it.
func TestApplyProviderDialogAction_RemoveAccountConfirmed_RemovesAndReloadsList(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{accs: []accounts.Account{{ID: "acct-2", Label: "Personal"}}}
	m := newCmdDrivenUI(ws)
	m.dialog.OpenDialog(dialog.NewAccountRemoveConfirm(m.com, "test-provider", accounts.Account{ID: "acct-1", Label: "Work"}))

	cmd, handled := m.applyProviderDialogAction(dialog.ActionRemoveAccountConfirmed{
		ProviderID: "test-provider", AccountID: "acct-1",
	})
	require.True(t, handled)
	require.False(t, m.dialog.ContainsDialog(dialog.AccountRemoveConfirmID), "the confirm dialog must close")
	require.Zero(t, ws.removeAccountCalls, "RemoveAccount must not run synchronously")
	require.NotNil(t, cmd)

	msg := findAccountsMsg(t, cmd, func(msg tea.Msg) bool {
		_, ok := msg.(dialog.ActionAccountsLoaded)
		return ok
	})
	loaded, ok := msg.(dialog.ActionAccountsLoaded)
	require.True(t, ok, "expected ActionAccountsLoaded, got %#v", msg)
	require.Equal(t, 1, ws.removeAccountCalls)
	require.Equal(t, "acct-1", ws.lastRemovedID)
	require.Equal(t, 1, ws.listAccountsCalls)
	require.Equal(t, ws.accs, loaded.Accounts)
}
