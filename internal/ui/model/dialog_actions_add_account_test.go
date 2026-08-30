package model

import (
	"testing"

	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// TestApplyProviderDialogAction_AddAccountForcesNewAccount is the wiring
// regression test for "Add account…": ActionAddAccount must construct the
// OAuth dialog with ForceNewAccount set, so RecordAccount always creates a
// new account rather than possibly refreshing the active one in place. The
// ordinary provider-selection path (ActionConfigureProvider) must leave it
// unset.
func TestApplyProviderDialogAction_AddAccountForcesNewAccount(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{}
	m := newCmdDrivenUI(ws)

	_, handled := m.applyProviderDialogAction(dialog.ActionAddAccount{ProviderID: dialog.CodexProviderID})
	require.True(t, handled)

	oa, ok := m.dialog.Dialog(dialog.OAuthID).(*dialog.OAuth)
	require.True(t, ok, "expected the Codex OAuth dialog to be open")
	require.True(t, oa.ForceNewAccount, "ActionAddAccount must force a new account")
}

// TestApplyProviderDialogAction_ConfigureProviderDoesNotForceNewAccount
// covers the ordinary path reached without going through "Add account…":
// it must not set ForceNewAccount, since a routine (re-)login there should
// still be able to update the active account in place.
func TestApplyProviderDialogAction_ConfigureProviderDoesNotForceNewAccount(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{}
	m := newCmdDrivenUI(ws)

	_, handled := m.applyProviderDialogAction(dialog.ActionConfigureProvider{ProviderID: dialog.CodexProviderID})
	require.True(t, handled)

	oa, ok := m.dialog.Dialog(dialog.OAuthID).(*dialog.OAuth)
	require.True(t, ok, "expected the Codex OAuth dialog to be open")
	require.False(t, oa.ForceNewAccount, "an ordinary provider-configuration session must not force a new account")
}
