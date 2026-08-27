package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func newTestAccountRemoveConfirm(t *testing.T, providerID string, account accounts.Account) *AccountRemoveConfirm {
	t.Helper()
	s := styles.SennitDark()
	com := &common.Common{Styles: &s}
	return NewAccountRemoveConfirm(com, providerID, account)
}

// TestAccountRemoveConfirm_DefaultIsNo pins the shared confirmDialog
// invariant this dialog relies on: it must not confirm on its own, so a
// stray Enter can't delete an account.
func TestAccountRemoveConfirm_DefaultIsNo(t *testing.T) {
	m := newTestAccountRemoveConfirm(t, "openai", accounts.Account{ID: "acct-1", Label: "Work"})

	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, ActionClose{}, action)
}

// TestAccountRemoveConfirm_YesReturnsConfirmedNoIO covers confirming: it
// must return ActionRemoveAccountConfirmed with the right IDs, doing no IO
// itself (per internal/ui/AGENTS.md's dialog rules — the caller performs
// the actual RemoveAccount call in a tea.Cmd).
func TestAccountRemoveConfirm_YesReturnsConfirmedNoIO(t *testing.T) {
	m := newTestAccountRemoveConfirm(t, "openai", accounts.Account{ID: "acct-1", Label: "Work"})

	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyLeft}) // select "Yep!"
	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, ActionRemoveAccountConfirmed{ProviderID: "openai", AccountID: "acct-1"}, action)
}

// TestAccountRemoveConfirm_CloseCancels covers Esc: it must close without
// producing a removal action.
func TestAccountRemoveConfirm_CloseCancels(t *testing.T) {
	m := newTestAccountRemoveConfirm(t, "openai", accounts.Account{ID: "acct-1", Label: "Work"})

	action := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, ActionClose{}, action)
}
