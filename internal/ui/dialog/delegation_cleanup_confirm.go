package dialog

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/ui/common"
)

// DelegationCleanupConfirmID identifies the cleanup confirmation dialog.
const DelegationCleanupConfirmID = "delegation-cleanup-confirm"

// ActionCleanupDelegationConfirmed is returned after cleanup is confirmed.
type ActionCleanupDelegationConfirmed struct{ ID string }

// DelegationCleanupConfirm guards isolated delegation cleanup.
type DelegationCleanupConfirm struct {
	*confirmDialog
	delegationID string
}

var _ Dialog = (*DelegationCleanupConfirm)(nil)

// NewDelegationCleanupConfirm creates a cleanup confirmation dialog.
func NewDelegationCleanupConfirm(com *common.Common, id, name string) *DelegationCleanupConfirm {
	d := &DelegationCleanupConfirm{delegationID: id}
	d.confirmDialog = newConfirmDialog(com, fmt.Sprintf("Clean up delegation %q?", name), []string{"Safe isolated work is removed. If cleanup is unsafe, its worktree and branch are preserved."}, func() Action { return ActionCleanupDelegationConfirmed{ID: id} }, false)
	return d
}

// ID implements Dialog.
func (*DelegationCleanupConfirm) ID() string { return DelegationCleanupConfirmID }

// HandleMsg intentionally leaves ctrl+c unbound: unlike Quit it must not confirm cleanup.
func (d *DelegationCleanupConfirm) HandleMsg(msg tea.Msg) Action {
	return d.confirmDialog.HandleMsg(msg)
}

// Draw implements Dialog.
func (d *DelegationCleanupConfirm) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	return d.confirmDialog.Draw(scr, area)
}
