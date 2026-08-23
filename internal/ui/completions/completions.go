package completions

import (
	"cmp"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/ordered"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/ui/list"
)

const (
	minHeight = 1
	maxHeight = 10
	minWidth  = 10
	maxWidth  = 100

	tierExactName = iota
	tierPrefixName
	tierPathSegment
	tierFallback
)

// borderFrameWidth/borderFrameHeight are the extra columns/rows the popup's
// border adds around the list content — 1 border cell on each side.
// Size() and Render() must agree on these so the popup is positioned for
// exactly what gets drawn.
const (
	borderFrameWidth  = 2
	borderFrameHeight = 2
)

// Scrollbar glyphs, mirroring styles.ScrollbarThumb/ScrollbarTrack. Kept as
// local literals rather than importing the styles package solely for two
// characters — this package otherwise only takes pre-built lipgloss.Style
// values from its caller.
const (
	scrollbarThumbGlyph = "┃"
	scrollbarTrackGlyph = "│"
)

// SelectionMsg is sent when a completion is selected.
type SelectionMsg[T any] struct {
	Value    T
	KeepOpen bool // If true, insert without closing.

	// InsertOnly is set for command completions picked with Tab: the
	// caller should insert the command's name into the editor rather than
	// running it (Enter runs it). Unused by file/resource completions.
	InsertOnly bool
}

// ClosedMsg is sent when the completions are closed.
type ClosedMsg struct{}

// CompletionItemsLoadedMsg is sent when files have been loaded for completions.
type CompletionItemsLoadedMsg struct {
	Files     []FileCompletionValue
	Resources []ResourceCompletionValue
}

// PopupStyles bundles every style the completions popup needs to draw
// itself: item rows, the description column, the frame, and the scrollbar.
// Passed as one value (rather than a growing list of positional
// lipgloss.Style args) to New and SetStyles.
type PopupStyles struct {
	// Normal/Focused style each row's background+foreground (focused =
	// the selected row, filled edge-to-edge — not just colored text).
	Normal, Focused lipgloss.Style
	// Match highlights the matched substring within a row's title.
	Match lipgloss.Style
	// Muted styles the description column.
	Muted lipgloss.Style
	// Border frames the whole popup (background + rounded border), toned
	// like a dialog so the popup reads as a solid panel rather than a
	// bare list floating over the chat.
	Border lipgloss.Style
	// ScrollbarThumb/ScrollbarTrack style the scroll indicator shown to
	// the right of the list when there are more items than fit on screen.
	ScrollbarThumb, ScrollbarTrack lipgloss.Style
}

// Completions represents the completions popup component.
type Completions struct {
	// Popup dimensions
	width  int
	height int

	// State
	open  bool
	query string

	// Key bindings
	keyMap KeyMap

	// List component
	list *list.FilterableList

	// Styling
	styles PopupStyles

	allItems []list.FilterableItem
	filtered []list.FilterableItem

	// capWidth is an additional ceiling on top of maxWidth, supplied by the
	// caller (e.g. the editor's available width) so the popup never asks
	// for more room than the terminal has. Zero means "no extra cap".
	capWidth int
}

type namePriorityRule struct {
	tier  int
	match func(pathLower, baseLower, stemLower, queryLower string) bool
}

var namePriorityRules = []namePriorityRule{
	{
		tier: tierExactName,
		match: func(_ string, baseLower, stemLower, queryLower string) bool {
			return baseLower == queryLower || stemLower == queryLower
		},
	},
	{
		tier: tierPrefixName,
		match: func(_ string, baseLower, _ string, queryLower string) bool {
			return strings.HasPrefix(baseLower, queryLower)
		},
	},
	{
		tier: tierPathSegment,
		match: func(pathLower, _ string, _ string, queryLower string) bool {
			return hasPathSegment(pathLower, queryLower)
		},
	},
}

// New creates a new completions component.
func New(styles PopupStyles) *Completions {
	l := list.NewFilterableList()
	l.SetGap(0)
	l.SetReverse(true)

	return &Completions{
		keyMap: DefaultKeyMap(),
		list:   l,
		styles: styles,
	}
}

// SetStyles updates the styles used when rendering completion items,
// including the items currently held: a theme can be switched while the
// popup is open, and items keep a copy of the styles they were built with.
func (c *Completions) SetStyles(styles PopupStyles) {
	c.styles = styles
	for _, item := range c.allItems {
		if s, ok := item.(*CompletionItem); ok {
			s.SetStyles(styles.Normal, styles.Focused, styles.Match, styles.Muted)
		}
	}
	c.list.InvalidateAll()
}

// IsOpen returns whether the completions popup is open.
func (c *Completions) IsOpen() bool {
	return c.open
}

// Query returns the current filter query.
func (c *Completions) Query() string {
	return c.query
}

// Size returns the popup's total on-screen footprint — list rows plus the
// scrollbar column (if shown) plus the border frame — so callers can
// position/clamp it without knowing the frame's exact size themselves.
func (c *Completions) Size() (width, height int) {
	visible := len(c.filtered)
	rows := min(visible, c.height)

	width = c.width
	if c.hasScrollbar() {
		width++
	}
	width += borderFrameWidth
	height = rows + borderFrameHeight
	return width, height
}

// SetMaxWidth caps the popup width to at most maxW columns, in addition to
// the package-wide maxWidth ceiling. Callers should call this before Open /
// OpenCommands with the currently available editor width, since the popup
// is positioned relative to the editor. A value <= 0 clears the extra cap.
func (c *Completions) SetMaxWidth(maxW int) {
	c.capWidth = maxW
}

// Open opens the completions with file items from the filesystem and MCP
// resources from loadResources, which the caller supplies bound to its
// workspace.Workspace (this package has no dependency of its own on
// internal/app or internal/workspace).
func (c *Completions) Open(depth, limit int, loadResources func() []ResourceCompletionValue) tea.Cmd {
	return func() tea.Msg {
		var msg CompletionItemsLoadedMsg
		var wg sync.WaitGroup
		wg.Go(func() {
			msg.Files = loadFiles(depth, limit)
		})
		wg.Go(func() {
			msg.Resources = loadResources()
		})
		wg.Wait()
		return msg
	}
}

// SetItems sets the files and MCP resources and rebuilds the merged list.
func (c *Completions) SetItems(files []FileCompletionValue, resources []ResourceCompletionValue) {
	items := make([]list.FilterableItem, 0, len(files)+len(resources))

	// Add files first.
	for _, file := range files {
		item := NewCompletionItem(
			file.Path,
			file,
			c.styles.Normal,
			c.styles.Focused,
			c.styles.Match,
		)
		items = append(items, item)
	}

	// Add MCP resources.
	for _, resource := range resources {
		item := NewCompletionItem(
			resource.MCPName+"/"+cmp.Or(resource.Title, resource.URI),
			resource,
			c.styles.Normal,
			c.styles.Focused,
			c.styles.Match,
		)
		items = append(items, item)
	}

	c.setAllItems(items)
}

// OpenCommands opens the popup with "/" command items. Unlike file/resource
// completions, the command list is already in memory (built from the same
// source as the Commands palette dialog), so this is synchronous — no
// loading command needed.
func (c *Completions) OpenCommands(values []CommandCompletionValue) {
	items := make([]list.FilterableItem, 0, len(values))
	for _, v := range values {
		items = append(items, NewCommandCompletionItem(v, c.styles.Normal, c.styles.Focused, c.styles.Match, c.styles.Muted))
	}
	c.setAllItems(items)
}

// setAllItems opens the popup with the given items, replacing whatever it
// held before, and resets query/selection/size state.
func (c *Completions) setAllItems(items []list.FilterableItem) {
	c.open = true
	c.query = ""
	c.allItems = items
	c.filtered = append([]list.FilterableItem(nil), items...)
	c.list.SetItems(c.filtered...)
	c.list.SetFilter("")
	c.list.Focus()

	// Width is sized once, from the full item set, and held fixed through
	// filtering below (see Filter) — recomputing it as the set narrows
	// would make the popup visibly shrink while the user types.
	c.width = c.computeWidth(items)
	c.updateSize()
}

// Close closes the completions popup.
func (c *Completions) Close() {
	c.open = false
}

// Filter filters the completions with the given query.
func (c *Completions) Filter(query string) {
	if !c.open {
		return
	}

	if query == c.query {
		return
	}

	c.query = query
	c.applyNamePriorityFilter(query)

	c.updateSize()
}

func (c *Completions) applyNamePriorityFilter(query string) {
	if query == "" {
		c.filtered = append([]list.FilterableItem(nil), c.allItems...)
		c.list.SetItems(c.filtered...)
		return
	}

	c.list.SetItems(c.allItems...)
	c.list.SetFilter(query)
	raw := c.list.FilteredItems()
	filtered := make([]list.FilterableItem, 0, len(raw))
	for _, item := range raw {
		filterable, ok := item.(list.FilterableItem)
		if !ok {
			continue
		}
		filtered = append(filtered, filterable)
	}

	queryLower := strings.ToLower(strings.TrimSpace(query))
	slices.SortStableFunc(filtered, func(a, b list.FilterableItem) int {
		return namePriorityTier(a.Filter(), queryLower) - namePriorityTier(b.Filter(), queryLower)
	})
	c.filtered = filtered
	c.list.SetItems(c.filtered...)
}

func namePriorityTier(path, queryLower string) int {
	if queryLower == "" {
		return tierFallback
	}

	pathLower := strings.ToLower(path)
	baseLower := strings.ToLower(filepath.Base(strings.ReplaceAll(path, `\`, `/`)))
	stemLower := strings.TrimSuffix(baseLower, filepath.Ext(baseLower))
	for _, rule := range namePriorityRules {
		if rule.match(pathLower, baseLower, stemLower, queryLower) {
			return rule.tier
		}
	}
	return tierFallback
}

func hasPathSegment(pathLower, queryLower string) bool {
	return slices.Contains(strings.FieldsFunc(pathLower, func(r rune) bool {
		return r == '/' || r == '\\'
	}), queryLower)
}

// updateSize recomputes the popup height for the current filtered set and
// re-selects/scrolls. The box width (c.width) is intentionally not touched
// here: it's fixed once at open time (see setAllItems) so the popup doesn't
// shrink as filtering narrows the set. The per-row width handed to the list
// does flex by one column whenever a scrollbar is showing, so rows never
// render underneath it.
func (c *Completions) updateSize() {
	items := c.filtered
	c.height = ordered.Clamp(len(items), int(minHeight), int(maxHeight))
	c.alignColumns(items)

	rowWidth := c.width
	if c.hasScrollbar() {
		rowWidth = max(0, c.width-1)
	}
	c.list.SetSize(rowWidth, c.height)
	c.list.SelectFirst()
	c.list.ScrollToSelected()
}

// hasScrollbar reports whether the current filtered set overflows the
// popup's viewport height, i.e. whether a scroll indicator is needed.
func (c *Completions) hasScrollbar() bool {
	return len(c.filtered) > c.height
}

// columnLayout scans items for the description column layout: the widest
// title (the column description text starts at, plus a 2-column gap) and
// the widest description. hasDesc is false when no item carries a
// description at all (e.g. the @-file popup), in which case titleColumn
// stays 0 and items fall back to single-column, title-only rendering.
func columnLayout(items []list.FilterableItem) (titleColumn, maxDesc int, hasDesc bool) {
	for _, item := range items {
		ci, ok := item.(*CompletionItem)
		if !ok {
			if t, ok := item.(interface{ Text() string }); ok {
				titleColumn = max(titleColumn, ansi.StringWidth(t.Text()))
			}
			continue
		}
		titleColumn = max(titleColumn, ansi.StringWidth(ci.text))
		if ci.desc != "" {
			hasDesc = true
			maxDesc = max(maxDesc, ansi.StringWidth(ci.desc))
		}
	}
	if hasDesc {
		titleColumn += 2
	}
	return titleColumn, maxDesc, hasDesc
}

// alignColumns recomputes the shared description column from the currently
// visible items and pushes it to each one, so titles/descriptions line up
// across whatever subset filtering has left on screen.
func (c *Completions) alignColumns(items []list.FilterableItem) {
	titleColumn, _, _ := columnLayout(items)
	for _, item := range items {
		if ci, ok := item.(*CompletionItem); ok {
			ci.SetTitleColumn(titleColumn)
		}
	}
}

// computeWidth measures the widest title/description column pair across
// items (see columnLayout) and clamps it to [minWidth, effective max],
// where the effective max is the package ceiling (maxWidth) further capped
// by capWidth if the caller set one via SetMaxWidth.
func (c *Completions) computeWidth(items []list.FilterableItem) int {
	titleColumn, maxDesc, hasDesc := columnLayout(items)
	width := titleColumn
	if hasDesc {
		width = titleColumn + maxDesc
	}

	upperBound := maxWidth
	if c.capWidth > 0 {
		upperBound = min(upperBound, c.capWidth)
	}
	upperBound = max(upperBound, int(minWidth))
	return ordered.Clamp(width+2, int(minWidth), upperBound)
}

// HasItems returns whether there are visible items.
func (c *Completions) HasItems() bool {
	return len(c.filtered) > 0
}

// Update handles key events for the completions.
func (c *Completions) Update(msg tea.KeyPressMsg) (tea.Msg, bool) {
	if !c.open {
		return nil, false
	}

	switch {
	case key.Matches(msg, c.keyMap.Up):
		c.selectPrev()
		return nil, true

	case key.Matches(msg, c.keyMap.Down):
		c.selectNext()
		return nil, true

	case key.Matches(msg, c.keyMap.UpInsert):
		c.selectPrev()
		return c.selectCurrent(true), true

	case key.Matches(msg, c.keyMap.DownInsert):
		c.selectNext()
		return c.selectCurrent(true), true

	case key.Matches(msg, c.keyMap.Select):
		// Tab on a command inserts its name into the editor instead of
		// running it, so commands that take arguments can be finished by
		// hand before Enter runs them.
		if msg.String() == "tab" {
			if v, ok := c.selectedCommand(); ok {
				c.open = false
				return SelectionMsg[CommandCompletionValue]{Value: v, InsertOnly: true}, true
			}
		}
		return c.selectCurrent(false), true

	case key.Matches(msg, c.keyMap.Cancel):
		c.Close()
		return ClosedMsg{}, true
	}

	return nil, false
}

// selectPrev selects the previous item with circular navigation.
func (c *Completions) selectPrev() {
	items := c.filtered
	if len(items) == 0 {
		return
	}
	if !c.list.SelectPrev() {
		c.list.WrapToEnd()
	}
	c.list.ScrollToSelected()
}

// selectNext selects the next item with circular navigation.
func (c *Completions) selectNext() {
	items := c.filtered
	if len(items) == 0 {
		return
	}
	if !c.list.SelectNext() {
		c.list.WrapToStart()
	}
	c.list.ScrollToSelected()
}

// selectCurrent returns a command with the currently selected item.
func (c *Completions) selectCurrent(keepOpen bool) tea.Msg {
	items := c.filtered
	if len(items) == 0 {
		return nil
	}

	selected := c.list.Selected()
	if selected < 0 || selected >= len(items) {
		return nil
	}

	item, ok := items[selected].(*CompletionItem)
	if !ok {
		return nil
	}

	if !keepOpen {
		c.open = false
	}

	switch item := item.Value().(type) {
	case ResourceCompletionValue:
		return SelectionMsg[ResourceCompletionValue]{
			Value:    item,
			KeepOpen: keepOpen,
		}
	case FileCompletionValue:
		return SelectionMsg[FileCompletionValue]{
			Value:    item,
			KeepOpen: keepOpen,
		}
	case CommandCompletionValue:
		return SelectionMsg[CommandCompletionValue]{
			Value:    item,
			KeepOpen: keepOpen,
		}
	default:
		return nil
	}
}

// selectedCommand returns the currently selected item's command value, if
// the popup is showing commands.
func (c *Completions) selectedCommand() (CommandCompletionValue, bool) {
	items := c.filtered
	selected := c.list.Selected()
	if selected < 0 || selected >= len(items) {
		return CommandCompletionValue{}, false
	}

	item, ok := items[selected].(*CompletionItem)
	if !ok {
		return CommandCompletionValue{}, false
	}

	v, ok := item.Value().(CommandCompletionValue)
	return v, ok
}

// Render renders the completions popup.
func (c *Completions) Render() string {
	if !c.open {
		return ""
	}

	items := c.filtered
	if len(items) == 0 {
		return ""
	}

	inner := c.list.List.Render()
	rows := min(len(items), c.height)

	// Width of the content area: list rows, plus one column for the
	// scrollbar when it's showing.
	contentWidth := c.width
	if sb := c.renderScrollbar(rows); sb != "" {
		inner = lipgloss.JoinHorizontal(lipgloss.Top, inner, sb)
		contentWidth++
	}

	// Frame the list in a solid, dialog-toned box (background + rounded
	// border) instead of a bare list floating over the chat. Width is the
	// box's *total* width (border included) — content is already sized to
	// exactly what's left over, so the border wraps it without reflowing.
	return c.styles.Border.Width(contentWidth + borderFrameWidth).Render(inner)
}

// renderScrollbar draws a thin vertical scroll indicator height rows tall,
// or "" if the current filtered set fits without scrolling.
func (c *Completions) renderScrollbar(height int) string {
	// c.list is always built with SetReverse(true) (see New): the
	// underlying items render bottom-to-top, so an offset measured from
	// the top of the (unreversed) items slice actually corresponds to
	// the *bottom* of what's on screen. Mirror it before handing it to
	// scrollbarThumbBounds, which assumes offset 0 means "scrolled to
	// the visual top" — without this the thumb tracks the wrong end of
	// the popup as the user scrolls.
	offset := c.list.Offset()
	if maxOffset := len(c.filtered) - height; maxOffset > 0 {
		offset = maxOffset - offset
	}
	thumbPos, thumbSize, ok := scrollbarThumbBounds(height, len(c.filtered), height, offset)
	if !ok {
		return ""
	}

	var sb strings.Builder
	for i := range height {
		if i > 0 {
			sb.WriteByte('\n')
		}
		if i >= thumbPos && i < thumbPos+thumbSize {
			sb.WriteString(c.styles.ScrollbarThumb.Render(scrollbarThumbGlyph))
		} else {
			sb.WriteString(c.styles.ScrollbarTrack.Render(scrollbarTrackGlyph))
		}
	}
	return sb.String()
}

// scrollbarThumbBounds returns the thumb's start row and size (in track
// cells) for a scrollbar representing contentSize rows of content in a
// viewportSize-row viewport scrolled to offset, within a track of the
// given height. ok is false when no thumb is needed (content fits).
//
// This mirrors common.ScrollbarThumbBounds; duplicated rather than
// imported so this package keeps taking only pre-built lipgloss.Style
// values from its caller instead of a dependency on internal/ui/common.
func scrollbarThumbBounds(height, contentSize, viewportSize, offset int) (start, size int, ok bool) {
	if height <= 0 || contentSize <= viewportSize {
		return 0, 0, false
	}

	thumbSize := max(1, height*viewportSize/contentSize)
	maxOffset := contentSize - viewportSize
	if maxOffset <= 0 {
		return 0, 0, false
	}

	trackSpace := height - thumbSize
	thumbPos := 0
	if trackSpace > 0 {
		thumbPos = min(trackSpace, offset*trackSpace/maxOffset)
	}
	return thumbPos, thumbSize, true
}

func loadFiles(depth, limit int) []FileCompletionValue {
	files, _, _ := fsext.ListDirectory(".", nil, depth, limit)
	slices.Sort(files)
	result := make([]FileCompletionValue, 0, len(files))
	for _, file := range files {
		result = append(result, FileCompletionValue{
			Path: strings.TrimPrefix(file, "./"),
		})
	}
	return result
}
