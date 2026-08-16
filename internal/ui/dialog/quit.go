package dialog

import (
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/ui/common"
)

// QuitID is the identifier for the quit dialog.
const QuitID = "quit"

// Quit represents a confirmation dialog for quitting the application.
type Quit struct{ *confirmDialog }

var _ Dialog = (*Quit)(nil)

// NewQuit creates a new quit confirmation dialog.
func NewQuit(com *common.Common) *Quit {
	return &Quit{newConfirmDialog(com, "Are you sure you want to quit?", []string{"To quit without confirmation", "press ctrl+c twice."}, func() Action { return ActionQuit{} }, true)}
}

// ID implements Dialog.
func (*Quit) ID() string { return QuitID }

// HandleMsg preserves Quit's ctrl+c-to-quit action contract.
func (q *Quit) HandleMsg(msg tea.Msg) Action { return q.confirmDialog.HandleMsg(msg) }

// Draw implements Dialog.
func (q *Quit) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	return q.confirmDialog.Draw(scr, area)
}
