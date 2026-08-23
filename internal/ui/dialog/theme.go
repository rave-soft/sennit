package dialog

import (
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

const (
	// ThemeID is the identifier for the theme selection dialog.
	ThemeID              = "theme"
	themeDialogMaxWidth  = 60
	themeDialogMinHeight = 8
	themeDialogMaxHeight = 16
)

// Theme is a dialog for picking the TUI color palette. Like [Reasoning] it
// is a thin wrapper around [selectDialog]; the list is short and fixed, so
// it sizes to its content between themeDialogMinHeight and
// themeDialogMaxHeight.
type Theme struct {
	*selectDialog
}

// ThemeItem represents one selectable palette.
type ThemeItem struct {
	list.BaseItem
	palette   styles.Palette
	isCurrent bool
	t         *styles.Styles
}

var (
	_ Dialog   = (*Theme)(nil)
	_ ListItem = (*ThemeItem)(nil)
)

// NewTheme creates a new theme selection dialog.
func NewTheme(com *common.Common) (*Theme, error) {
	sd, err := newSelectDialog(com, selectDialogConfig{
		id:            ThemeID,
		title:         "Select Theme",
		maxWidth:      themeDialogMaxWidth,
		dynamicHeight: true,
		minHeight:     themeDialogMinHeight,
		maxHeight:     themeDialogMaxHeight,
		buildItems:    func() ([]list.FilterableItem, int, error) { return themeItems(com) },
		onSelect: func(id string) Action {
			return ActionSelectTheme{ID: id}
		},
		// Walking the list repaints the UI in the highlighted palette, so
		// the choice is made by looking at it rather than by its name. The
		// UI restores the previous palette if the dialog is closed without
		// a selection.
		onMove: func(id string) Action {
			return ActionPreviewTheme{ID: id}
		},
	})
	if err != nil {
		return nil, err
	}
	return &Theme{selectDialog: sd}, nil
}

// themeItems builds the palette list, returning the index of the palette
// currently in use so the dialog opens with it selected.
func themeItems(com *common.Common) ([]list.FilterableItem, int, error) {
	current := styles.PaletteByID(common.ThemeID(com.Workspace)).ID

	palettes := styles.Palettes()
	items := make([]list.FilterableItem, 0, len(palettes))
	selectedIndex := 0
	for i, p := range palettes {
		items = append(items, &ThemeItem{
			BaseItem:  list.NewBaseItem(),
			palette:   p,
			isCurrent: p.ID == current,
			t:         com.Styles,
		})
		if p.ID == current {
			selectedIndex = i
		}
	}
	return items, selectedIndex, nil
}

// Filter matches on both the display name and the persisted ID, so typing
// either "ink" or "ink-sage" finds the palette.
func (t *ThemeItem) Filter() string {
	return t.palette.Name + " " + t.palette.ID
}

// ID returns the palette identifier.
func (t *ThemeItem) ID() string {
	return t.palette.ID
}

// Render returns the string representation of the theme item.
func (t *ThemeItem) Render(width int) string {
	info := t.palette.Description
	if t.isCurrent {
		info = "current"
	}
	styles := defaultListItemStyles(t.t)
	return renderItem(styles, t.palette.Name, info, t.Focused(), width, t.Cache(), t.Match())
}
