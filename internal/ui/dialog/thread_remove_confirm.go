package dialog

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/ui/common"
)

// ThreadRemoveConfirmID is the identifier for the thread-remove confirmation dialog.
const ThreadRemoveConfirmID = "thread-remove-confirm"

// ActionRemoveThreadConfirmed is returned once the user confirms removing the thread.
type ActionRemoveThreadConfirmed struct{ ID string }

// ThreadRemoveConfirm is a Yes/No confirmation dialog guarding thread removal.
type ThreadRemoveConfirm struct {
	*confirmDialog
	threadID string
}

var _ Dialog = (*ThreadRemoveConfirm)(nil)

// NewThreadRemoveConfirm creates a confirmation dialog for removing the thread identified by id, displayed as name.
func NewThreadRemoveConfirm(com *common.Common, id, name string) *ThreadRemoveConfirm {
	d := &ThreadRemoveConfirm{threadID: id}
	d.confirmDialog = newConfirmDialog(com, fmt.Sprintf("Remove thread %q?", name), []string{"This deletes its worktree and record."}, func() Action { return ActionRemoveThreadConfirmed{ID: id} }, false)
	return d
}

// ID implements Dialog.
func (*ThreadRemoveConfirm) ID() string { return ThreadRemoveConfirmID }

// HandleMsg intentionally leaves ctrl+c unbound: unlike Quit it must not confirm removal.
func (d *ThreadRemoveConfirm) HandleMsg(msg tea.Msg) Action { return d.confirmDialog.HandleMsg(msg) }

// Draw implements Dialog.
func (d *ThreadRemoveConfirm) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	return d.confirmDialog.Draw(scr, area)
}
