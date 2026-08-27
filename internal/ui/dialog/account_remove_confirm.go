package dialog

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/ui/common"
)

// AccountRemoveConfirmID is the identifier for the account-remove
// confirmation dialog.
const AccountRemoveConfirmID = "account-remove-confirm"

// AccountRemoveConfirm is a Yes/No confirmation dialog guarding account
// removal, mirroring [ThreadRemoveConfirm]. It does no IO itself: on
// confirmation it returns [ActionRemoveAccountConfirmed], and the caller
// (ui.go) performs the actual RemoveAccount call in a tea.Cmd.
type AccountRemoveConfirm struct {
	*confirmDialog
	providerID string
	accountID  string
}

var _ Dialog = (*AccountRemoveConfirm)(nil)

// NewAccountRemoveConfirm creates a confirmation dialog for removing
// account, one of providerID's stored accounts.
func NewAccountRemoveConfirm(com *common.Common, providerID string, account accounts.Account) *AccountRemoveConfirm {
	label := account.Label
	if label == "" {
		label = account.ID
	}
	d := &AccountRemoveConfirm{providerID: providerID, accountID: account.ID}
	d.confirmDialog = newConfirmDialog(com, fmt.Sprintf("Remove account %q?", label), nil, func() Action {
		return ActionRemoveAccountConfirmed{ProviderID: providerID, AccountID: account.ID}
	}, false)
	return d
}

// ID implements Dialog.
func (*AccountRemoveConfirm) ID() string { return AccountRemoveConfirmID }

// HandleMsg intentionally leaves ctrl+c unbound: unlike Quit it must not
// confirm removal.
func (d *AccountRemoveConfirm) HandleMsg(msg tea.Msg) Action { return d.confirmDialog.HandleMsg(msg) }

// Draw implements Dialog.
func (d *AccountRemoveConfirm) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	return d.confirmDialog.Draw(scr, area)
}
