package dialog

import (
	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/list"
)

// selectDialogConfig describes a filterable single-select list dialog.
// Notifications and Reasoning are thin wrappers around [selectDialog] built
// from one of these.
type selectDialogConfig struct {
	id               string
	title            string
	maxWidth         int
	inputPlaceholder string
	// selectKeys overrides the default confirmation bindings. It lets dialogs
	// retain domain-specific selection keys without changing shared navigation.
	selectKeys []string

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
	Base
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
	d := &selectDialog{Base: NewBase(com, cfg.maxWidth), com: com, cfg: cfg}

	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()
	d.help = h

	d.list = list.NewFilterableList()
	d.list.Focus()

	d.input = textinput.New()
	d.input.SetVirtualCursor(false)
	d.input.Placeholder = cfg.inputPlaceholder
	if d.input.Placeholder == "" {
		d.input.Placeholder = "Type to filter"
	}
	d.input.SetStyles(com.Styles.TextInput)
	d.input.Focus()

	selectKeys := cfg.selectKeys
	if len(selectKeys) == 0 {
		selectKeys = []string{"enter", "ctrl+y"}
	}
	d.keyMap.Select = key.NewBinding(
		key.WithKeys(selectKeys...),
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
	return d.replaceItems(d.cfg.buildItems)
}

// replaceItems rebuilds the shared list and preserves selection by stable ID
// when the caller changes its source items asynchronously or on resize.
func (d *selectDialog) replaceItems(build func() ([]list.FilterableItem, int, error)) error {
	selectedID := d.selectedID()
	items, selectedIndex, err := build()
	if err != nil {
		return err
	}
	d.list.SetItems(items...)
	d.list.SetFilter("")
	d.input.SetValue("")
	if selectedID != "" {
		for i, item := range d.list.FilteredItems() {
			if selectable, ok := item.(ListItem); ok && selectable.ID() == selectedID {
				selectedIndex = i
				break
			}
		}
	}
	d.list.SetSelected(selectedIndex)
	d.list.ScrollToSelected()
	return nil
}

type selectDialogLayout struct {
	width, innerWidth, listHeight, listTotalHeight, listWidth int
}

// layout applies the common centered select-dialog geometry: Base width,
// filter input width and either fixed or content-sized list viewport. Dialogs
// with additional title state reuse it instead of copying this bookkeeping.
func (d *selectDialog) layout(area uv.Rectangle, height int, dynamic bool) selectDialogLayout {
	t := d.com.Styles
	d.Resize(area)
	layout := selectDialogLayout{width: d.Width(), innerWidth: d.InnerWidth()}
	d.input.SetWidth(dialogInputTextWidth(t, d.input, layout.innerWidth))
	if dynamic {
		layout.listHeight, layout.listTotalHeight, layout.listWidth = sizeDialogList(t, d.list, layout.innerWidth, height)
		return layout
	}
	heightOffset := t.Dialog.Title.GetVerticalFrameSize() + titleContentHeight +
		t.Dialog.InputPrompt.GetVerticalFrameSize() + inputContentHeight +
		t.Dialog.HelpView.GetVerticalFrameSize() + t.Dialog.View.GetVerticalFrameSize()
	layout.listWidth = layout.innerWidth
	d.list.SetSize(layout.innerWidth, max(0, height-heightOffset))
	layout.listHeight = d.list.Height()
	return layout
}

func (d *selectDialog) selectedID() string {
	item, ok := d.list.SelectedItem().(ListItem)
	if !ok || item == nil {
		return ""
	}
	return item.ID()
}

// handleSelect runs the configured selection binding. Composite dialogs can
// call it before their own shortcut handling when selection must take priority.
func (d *selectDialog) handleSelect(msg tea.KeyPressMsg, selectAction func(ListItem) Action) (Action, bool) {
	if !key.Matches(msg, d.keyMap.Select) {
		return nil, false
	}
	item, ok := d.list.SelectedItem().(ListItem)
	if !ok || item == nil {
		return nil, true
	}
	return selectAction(item), true
}

// handleNavigation handles the common close, circular navigation, selection,
// and filter-input behavior. Callers with extra modes can delegate their
// normal state here and keep only their exceptional transitions locally.
func (d *selectDialog) handleNavigation(msg tea.KeyPressMsg, selectAction func(ListItem) Action) Action {
	switch {
	case key.Matches(msg, d.keyMap.Close):
		return ActionClose{}
	case key.Matches(msg, d.keyMap.Previous):
		d.list.Focus()
		if d.list.IsSelectedFirst() {
			d.list.SelectLast()
			d.list.ScrollToBottom()
		} else {
			d.list.SelectPrev()
			d.list.ScrollToSelected()
		}
		return nil
	case key.Matches(msg, d.keyMap.Next):
		d.list.Focus()
		if d.list.IsSelectedLast() {
			d.list.SelectFirst()
			d.list.ScrollToTop()
		} else {
			d.list.SelectNext()
			d.list.ScrollToSelected()
		}
		return nil
	default:
		if action, handled := d.handleSelect(msg, selectAction); handled {
			return action
		}

		var cmd tea.Cmd
		d.input, cmd = d.input.Update(msg)
		d.list.SetFilter(d.input.Value())
		d.list.ScrollToTop()
		d.list.SetSelected(0)
		return ActionCmd{Cmd: cmd}
	}
}

// ID implements Dialog.
func (d *selectDialog) ID() string {
	return d.cfg.id
}

// HandleMsg implements [Dialog].
func (d *selectDialog) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return d.handleNavigation(msg, func(item ListItem) Action {
			return d.cfg.onSelect(item.ID())
		})
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
	d.Resize(area)
	width := d.Width()
	innerWidth := d.InnerWidth()
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
