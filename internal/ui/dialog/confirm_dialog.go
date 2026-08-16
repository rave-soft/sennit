package dialog

import (
	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/ui/common"
)

// confirmDialog contains the shared behavior and layout of the application's
// two-button confirmation dialogs. It deliberately remains private: callers
// retain their existing dialog types and action contracts.
type confirmDialog struct {
	com        *common.Common
	selectedNo bool
	question   string
	hintLines  []string
	yes        func() Action
	ctrlCYes   bool
	keyMap     struct {
		LeftRight, EnterSpace, Yes, No, Tab, Close, Quit key.Binding
	}
}

func newConfirmDialog(com *common.Common, question string, hintLines []string, yes func() Action, ctrlCYes bool) *confirmDialog {
	d := &confirmDialog{com: com, selectedNo: true, question: question, hintLines: hintLines, yes: yes, ctrlCYes: ctrlCYes}
	d.keyMap.LeftRight = key.NewBinding(key.WithKeys("left", "right"), key.WithHelp("←/→", "switch options"))
	d.keyMap.EnterSpace = key.NewBinding(key.WithKeys("enter", "space"), key.WithHelp("enter/space", "confirm"))
	d.keyMap.Yes = key.NewBinding(key.WithKeys("y", "Y"), key.WithHelp("y/Y", "yes"))
	d.keyMap.No = key.NewBinding(key.WithKeys("n", "N"), key.WithHelp("n/N", "no"))
	d.keyMap.Tab = key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "switch options"))
	d.keyMap.Close = CloseKey
	if ctrlCYes {
		d.keyMap.Quit = key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit"))
	}
	return d
}

func (d *confirmDialog) HandleMsg(msg tea.Msg) Action {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	switch {
	case d.ctrlCYes && key.Matches(keyMsg, d.keyMap.Quit):
		return d.yes()
	case key.Matches(keyMsg, d.keyMap.Close, d.keyMap.No):
		return ActionClose{}
	case key.Matches(keyMsg, d.keyMap.LeftRight, d.keyMap.Tab):
		d.selectedNo = !d.selectedNo
	case key.Matches(keyMsg, d.keyMap.EnterSpace):
		if d.selectedNo {
			return ActionClose{}
		}
		return d.yes()
	case key.Matches(keyMsg, d.keyMap.Yes):
		return d.yes()
	}
	return nil
}

func (d *confirmDialog) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	baseStyle, hintStyle := d.com.Styles.Dialog.Quit.Content, d.com.Styles.Dialog.Quit.Hint
	buttons := common.ButtonGroup(d.com.Styles, []common.ButtonOpts{{Text: "Yep!", Selected: !d.selectedNo, Padding: 3}, {Text: "Nope", Selected: d.selectedNo, Padding: 3}}, " ")
	lines := []string{d.question, "", buttons}
	if len(d.hintLines) > 0 {
		lines = append(lines, "")
		for _, hint := range d.hintLines {
			lines = append(lines, hintStyle.Render(hint))
		}
	}
	content := baseStyle.Render(lipgloss.JoinVertical(lipgloss.Center, lines...))
	frame := d.com.Styles.Dialog.Quit.Frame
	if area.Dx()-frame.GetHorizontalBorderSize() < lipgloss.Width(content) {
		frame = frame.Padding(1, 0)
	}
	DrawCenter(scr, area, frame.Render(content))
	return nil
}

func (d *confirmDialog) ShortHelp() []key.Binding {
	return []key.Binding{d.keyMap.LeftRight, d.keyMap.EnterSpace}
}

func (d *confirmDialog) FullHelp() [][]key.Binding {
	return [][]key.Binding{{d.keyMap.LeftRight, d.keyMap.EnterSpace, d.keyMap.Yes, d.keyMap.No}, {d.keyMap.Tab, d.keyMap.Close}}
}

var _ help.KeyMap = (*confirmDialog)(nil)
