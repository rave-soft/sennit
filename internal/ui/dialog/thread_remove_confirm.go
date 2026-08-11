package dialog

import (
	"fmt"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/ui/common"
)

// ThreadRemoveConfirmID is the identifier for the thread-remove
// confirmation dialog.
const ThreadRemoveConfirmID = "thread-remove-confirm"

// ActionRemoveThreadConfirmed is returned once the user confirms removing
// the thread named Name (ID is what the caller actually removes; Name is
// only for display).
type ActionRemoveThreadConfirmed struct {
	ID string
}

// ThreadRemoveConfirm is a Yes/No confirmation dialog guarding thread
// removal, mirroring Quit's button-pair layout.
type ThreadRemoveConfirm struct {
	com        *common.Common
	threadID   string
	threadName string
	selectedNo bool
	keyMap     struct {
		LeftRight,
		EnterSpace,
		Yes,
		No,
		Tab,
		Close key.Binding
	}
}

var _ Dialog = (*ThreadRemoveConfirm)(nil)

// NewThreadRemoveConfirm creates a confirmation dialog for removing the
// thread identified by id, displayed as name.
func NewThreadRemoveConfirm(com *common.Common, id, name string) *ThreadRemoveConfirm {
	d := &ThreadRemoveConfirm{
		com:        com,
		threadID:   id,
		threadName: name,
		selectedNo: true,
	}
	d.keyMap.LeftRight = key.NewBinding(
		key.WithKeys("left", "right"),
		key.WithHelp("←/→", "switch options"),
	)
	d.keyMap.EnterSpace = key.NewBinding(
		key.WithKeys("enter", " "),
		key.WithHelp("enter/space", "confirm"),
	)
	d.keyMap.Yes = key.NewBinding(
		key.WithKeys("y", "Y"),
		key.WithHelp("y/Y", "yes"),
	)
	d.keyMap.No = key.NewBinding(
		key.WithKeys("n", "N"),
		key.WithHelp("n/N", "no"),
	)
	d.keyMap.Tab = key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "switch options"),
	)
	d.keyMap.Close = CloseKey
	return d
}

// ID implements Dialog.
func (d *ThreadRemoveConfirm) ID() string { return ThreadRemoveConfirmID }

// HandleMsg implements Dialog.
func (d *ThreadRemoveConfirm) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.keyMap.Close, d.keyMap.No):
			return ActionClose{}
		case key.Matches(msg, d.keyMap.LeftRight, d.keyMap.Tab):
			d.selectedNo = !d.selectedNo
		case key.Matches(msg, d.keyMap.EnterSpace):
			if !d.selectedNo {
				return ActionRemoveThreadConfirmed{ID: d.threadID}
			}
			return ActionClose{}
		case key.Matches(msg, d.keyMap.Yes):
			return ActionRemoveThreadConfirmed{ID: d.threadID}
		}
	}
	return nil
}

// Draw implements Dialog.
func (d *ThreadRemoveConfirm) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	question := fmt.Sprintf("Remove thread %q?", d.threadName)
	hint := "This deletes its worktree and record."

	baseStyle := d.com.Styles.Dialog.Quit.Content
	hintStyle := d.com.Styles.Dialog.Quit.Hint

	buttonOpts := []common.ButtonOpts{
		{Text: "Yep!", Selected: !d.selectedNo, Padding: 3},
		{Text: "Nope", Selected: d.selectedNo, Padding: 3},
	}
	buttons := common.ButtonGroup(d.com.Styles, buttonOpts, " ")
	content := baseStyle.Render(
		lipgloss.JoinVertical(
			lipgloss.Center,
			question,
			"",
			buttons,
			"",
			hintStyle.Render(hint),
		),
	)

	frameStyle := d.com.Styles.Dialog.Quit.Frame
	maxWidth := area.Dx() - frameStyle.GetHorizontalBorderSize()
	if maxWidth < lipgloss.Width(content) {
		frameStyle = frameStyle.Padding(1, 0)
	}
	view := frameStyle.Render(content)
	DrawCenter(scr, area, view)
	return nil
}

// ShortHelp implements help.KeyMap.
func (d *ThreadRemoveConfirm) ShortHelp() []key.Binding {
	return []key.Binding{d.keyMap.LeftRight, d.keyMap.EnterSpace}
}

// FullHelp implements help.KeyMap.
func (d *ThreadRemoveConfirm) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{d.keyMap.LeftRight, d.keyMap.EnterSpace, d.keyMap.Yes, d.keyMap.No},
		{d.keyMap.Tab, d.keyMap.Close},
	}
}
