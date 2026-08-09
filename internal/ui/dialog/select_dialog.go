package dialog

import (
	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/list"
)

// selectDialogConfig describes a filterable single-select list dialog.
// Notifications and Reasoning are thin wrappers around [selectDialog] built
// from one of these.
type selectDialogConfig struct {
	id       string
	title    string
	maxWidth int

	// dynamicHeight sizes the dialog to its content (via sizeDialogList and
	// joinScrollbar), clamped to [minHeight, maxHeight]. When false, the
	// dialog uses a fixed maxHeight with no min-height and no scrollbar.
	dynamicHeight bool
	minHeight     int
	maxHeight     int

	// buildItems (re)populates the list. It returns the items to show and
	// the index that should start selected.
	buildItems func() ([]list.FilterableItem, int, error)

	// onSelect builds the Action to return when the item with the given ID
	// is chosen.
	onSelect func(id string) Action
}

// selectDialog is the shared machinery behind the notification style and
// reasoning effort pickers: a filterable, single-select list with a title,
// text input, and help footer. Behavior specific to each caller (item
// content, sizing strategy, selection action) is supplied via
// [selectDialogConfig].
type selectDialog struct {
	com   *common.Common
	cfg   selectDialogConfig
	help  help.Model
	list  *list.FilterableList
	input textinput.Model

	keyMap struct {
		Select   key.Binding
		Next     key.Binding
		Previous key.Binding
		UpDown   key.Binding
		Close    key.Binding
	}
}

var _ Dialog = (*selectDialog)(nil)

// newSelectDialog builds a [selectDialog] and populates it via
// cfg.buildItems.
func newSelectDialog(com *common.Common, cfg selectDialogConfig) (*selectDialog, error) {
	d := &selectDialog{com: com, cfg: cfg}

	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()
	d.help = h

	d.list = list.NewFilterableList()
	d.list.Focus()

	d.input = textinput.New()
	d.input.SetVirtualCursor(false)
	d.input.Placeholder = "Type to filter"
	d.input.SetStyles(com.Styles.TextInput)
	d.input.Focus()

	d.keyMap.Select = key.NewBinding(
		key.WithKeys("enter", "ctrl+y"),
		key.WithHelp("enter", "confirm"),
	)
	d.keyMap.Next = key.NewBinding(
		key.WithKeys("down", "ctrl+n"),
		key.WithHelp("↓", "next item"),
	)
	d.keyMap.Previous = key.NewBinding(
		key.WithKeys("up", "ctrl+p"),
		key.WithHelp("↑", "previous item"),
	)
	d.keyMap.UpDown = key.NewBinding(
		key.WithKeys("up", "down"),
		key.WithHelp("↑/↓", "choose"),
	)
	d.keyMap.Close = CloseKey

	if err := d.reloadItems(); err != nil {
		return nil, err
	}

	return d, nil
}

// reloadItems re-runs cfg.buildItems and resets the list to its result.
func (d *selectDialog) reloadItems() error {
	items, selectedIndex, err := d.cfg.buildItems()
	if err != nil {
		return err
	}
	d.list.SetItems(items...)
	d.list.SetSelected(selectedIndex)
	d.list.ScrollToSelected()
	return nil
}

// ID implements Dialog.
func (d *selectDialog) ID() string {
	return d.cfg.id
}

// HandleMsg implements [Dialog].
func (d *selectDialog) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, d.keyMap.Previous):
			d.list.Focus()
			if d.list.IsSelectedFirst() {
				d.list.SelectLast()
				d.list.ScrollToBottom()
				break
			}
			d.list.SelectPrev()
			d.list.ScrollToSelected()
		case key.Matches(msg, d.keyMap.Next):
			d.list.Focus()
			if d.list.IsSelectedLast() {
				d.list.SelectFirst()
				d.list.ScrollToTop()
				break
			}
			d.list.SelectNext()
			d.list.ScrollToSelected()
		case key.Matches(msg, d.keyMap.Select):
			selectedItem := d.list.SelectedItem()
			if selectedItem == nil {
				break
			}
			item, ok := selectedItem.(ListItem)
			if !ok {
				break
			}
			return d.cfg.onSelect(item.ID())
		default:
			var cmd tea.Cmd
			d.input, cmd = d.input.Update(msg)
			value := d.input.Value()
			d.list.SetFilter(value)
			d.list.ScrollToTop()
			d.list.SetSelected(0)
			return ActionCmd{cmd}
		}
	}
	return nil
}

// Cursor returns the cursor position relative to the dialog.
func (d *selectDialog) Cursor() *tea.Cursor {
	return InputCursor(d.com.Styles, d.input.Cursor())
}

// Draw implements [Dialog].
func (d *selectDialog) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	width := max(0, min(d.cfg.maxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()
	heightOffset := t.Dialog.Title.GetVerticalFrameSize() + titleContentHeight +
		t.Dialog.InputPrompt.GetVerticalFrameSize() + inputContentHeight +
		t.Dialog.HelpView.GetVerticalFrameSize() +
		t.Dialog.View.GetVerticalFrameSize()

	d.input.SetWidth(dialogInputTextWidth(t, d.input, innerWidth))

	var listHeight, listTotalHeight int
	if d.cfg.dynamicHeight {
		// Size the dialog to fit the list content, clamped to min/max bounds.
		desiredHeight := heightOffset + d.list.TotalHeight()
		maxAvailable := area.Dy() - t.Dialog.View.GetVerticalBorderSize()
		height := max(d.cfg.minHeight, min(d.cfg.maxHeight, desiredHeight, maxAvailable))
		listHeight, listTotalHeight, _ = sizeDialogList(t, d.list, innerWidth, height)
	} else {
		height := max(0, min(d.cfg.maxHeight, area.Dy()-t.Dialog.View.GetVerticalBorderSize()))
		d.list.SetSize(innerWidth, max(0, height-heightOffset))
	}

	rc := NewRenderContext(t, width)
	rc.Title = d.cfg.title
	inputView := t.Dialog.InputPrompt.Render(d.input.View())
	rc.AddPart(inputView)

	visibleCount := len(d.list.FilteredItems())
	if d.list.Height() >= visibleCount {
		d.list.ScrollToTop()
	} else {
		d.list.ScrollToSelected()
	}

	listView := t.Dialog.List.Height(d.list.Height()).Render(d.list.Render())
	if d.cfg.dynamicHeight {
		listView = joinScrollbar(t, listView, listHeight, listTotalHeight, listHeight, d.list.Offset())
	}
	rc.AddPart(listView)
	rc.Help = renderDialogHelp(t, &d.help, d, innerWidth)

	view := rc.Render()

	cur := d.Cursor()
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

// ShortHelp implements [help.KeyMap].
func (d *selectDialog) ShortHelp() []key.Binding {
	return []key.Binding{
		d.keyMap.UpDown,
		d.keyMap.Select,
		d.keyMap.Close,
	}
}

// FullHelp implements [help.KeyMap].
func (d *selectDialog) FullHelp() [][]key.Binding {
	m := [][]key.Binding{}
	slice := []key.Binding{
		d.keyMap.Select,
		d.keyMap.Next,
		d.keyMap.Previous,
		d.keyMap.Close,
	}
	for i := 0; i < len(slice); i += 4 {
		end := min(i+4, len(slice))
		m = append(m, slice[i:end])
	}
	return m
}
