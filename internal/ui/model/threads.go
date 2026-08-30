package model

// threadsDashboard is the threads administration screen. It wraps a
// list.List over the memoized thread cache (threads_cache.go) the same way
// Chat wraps a list.List over messages: items are rebuilt from the cache
// on load/event, never fetched directly from Draw/HandleKey.
//
// The screen is operated, not just read, so it is built as a table with
// chrome around it rather than a bare list: a toolbar whose buttons
// enable and disable with the selection, status filter tabs, and a detail
// pane carrying the fields a row cannot hold (the full goal, and the error
// a failed thread left behind). Buttons and shortcuts both go through
// runAction, so what a button offers is exactly what a key can do. The
// chrome that responds to a pointer is described in threads_admin.go.
//
// Statuses render as a colored cell rather than a live per-row spinner: an
// spin.Anim only advances when something drives it with spin.StepMsg on a
// ticker (see chat/assistant.go, driven by Chat.Animate from the main
// Update loop), and this screen has no such driver, so an anim here would
// render a permanently frozen first frame.

import (
	"fmt"
	"image"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/ultraviolet/layout"
	"github.com/charmbracelet/x/ansi"
	"github.com/dustin/go-humanize"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/presentation"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// enterThreadMsg requests attaching to a thread's own workspace/session.
// Consumed by the router (root.go).
type enterThreadMsg struct {
	id        string
	sessionID string
	// name is the thread's own name, carried through so the attached UI
	// can show it as a breadcrumb without a second lookup.
	name string
}

// openThreadCreateMsg requests opening the create-thread dialog. The dialog
// itself is implemented in a later step; this is a placeholder message so
// the dashboard can be wired up ahead of it.
type openThreadCreateMsg struct{}

// mergeThreadMsg requests merging a completed thread's branch back into its
// base branch.
type mergeThreadMsg struct {
	id string
}

// removeThreadMsg requests removing a thread, already confirmed by the
// thread-remove-confirm dialog (see confirmRemoveThreadMsg).
type removeThreadMsg struct {
	id string
}

// confirmRemoveThreadMsg requests opening the remove-confirmation dialog
// for a thread. Consumed by the router (root.go), which pushes
// dialog.NewThreadRemoveConfirm onto dashboardDialog; that dialog's
// ActionRemoveThreadConfirmed is what actually sends removeThreadMsg.
type confirmRemoveThreadMsg struct {
	id, name string
}

// leaveDashboardMsg requests closing the dashboard and returning to the
// main screen — what the Back button raises. Consumed by the router
// (root.go), which owns the screen stack.
type leaveDashboardMsg struct{}

// cancelDelegationMsg requests cancelling a delegation's (task or thread)
// in-flight run. Consumed by the router (root.go), which calls
// Workspace.CancelTask or Workspace.CancelThread depending on kind — a
// thread's cancel leaves its worktree and branch on disk, unlike Remove's
// full teardown.
type cancelDelegationMsg struct {
	id, kind string
}

// threadsKeyMap holds the key bindings local to the threads dashboard. It
// is intentionally separate from the app-wide KeyMap in keys.go: these
// bindings only apply while the dashboard has focus.
type threadsKeyMap struct {
	Up         key.Binding
	Down       key.Binding
	Enter      key.Binding
	New        key.Binding
	Merge      key.Binding
	Remove     key.Binding
	Cancel     key.Binding
	Reload     key.Binding
	NextFilter key.Binding
	AllFilter  key.Binding
}

// defaultThreadsKeyMap returns the threads dashboard's key bindings.
func defaultThreadsKeyMap() threadsKeyMap {
	return threadsKeyMap{
		Up: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "down"),
		),
		Enter: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "open"),
		),
		New: key.NewBinding(
			key.WithKeys("n"),
			key.WithHelp("n", "new"),
		),
		Merge: key.NewBinding(
			key.WithKeys("m"),
			key.WithHelp("m", "merge"),
		),
		Remove: key.NewBinding(
			key.WithKeys("x", "d"),
			key.WithHelp("x/d", "remove"),
		),
		Cancel: key.NewBinding(
			key.WithKeys("c"),
			key.WithHelp("c", "cancel"),
		),
		Reload: key.NewBinding(
			key.WithKeys("r"),
			key.WithHelp("r", "refresh"),
		),
		NextFilter: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "filter"),
		),
		AllFilter: key.NewBinding(
			key.WithKeys("a"),
			key.WithHelp("a", "all"),
		),
	}
}

// ShortHelp implements [help.KeyMap] so the footer can be built with the
// same shared help machinery other views use.
func (k threadsKeyMap) ShortHelp() []key.Binding {
	// Up/Down are deliberately absent: arrow keys in a list need no
	// explaining, and the footer is one line — every binding it lists is
	// one an operator might not guess.
	return []key.Binding{
		k.Enter, k.New, k.Merge, k.Cancel, k.Remove, k.Reload, k.NextFilter,
	}
}

// threadsDashboard is the threads administration screen: a title bar with
// a Back button, a toolbar of action buttons, status filter tabs, the
// thread table, a detail pane for the selected thread, and a help footer.
//
// Buttons and tabs are hit-tested from zones recomputed on every Draw
// (threadsHitZone), the same recompute-don't-cache approach the session
// panel's row hit-test uses: the layout depends on size, filter, and
// selection, so a stale rect would send a click to the wrong action.
type threadsDashboard struct {
	com    *common.Common
	list   *list.List
	cache  *threadListCache
	keyMap threadsKeyMap

	filter threadsFilter
	// visible is the filtered slice the list currently shows, in list
	// order — the bridge between a selected row index and the thread it
	// stands for.
	visible []proto.Thread

	// zones, hovered and columns are draw-time state read back by the
	// mouse handlers between frames.
	zones   []threadsHitZone
	hovered int // index into zones, -1 for none
	columns threadsColumns
	// listRect is where the table body landed, for row hit-testing.
	listRect image.Rectangle

	width, height int
	active        bool

	// styleRev is the palette build the cached rows were rendered from;
	// see Draw.
	styleRev uint64
}

// newThreadsDashboard creates a new threads dashboard over the given shared
// thread list cache (threads_cache.go) — the same instance the main UI's
// dock and header badge read, so all three stay in sync off a single
// ListThreads round trip.
func newThreadsDashboard(com *common.Common, cache *threadListCache) *threadsDashboard {
	l := list.NewList()
	l.RegisterRenderCallback(list.FocusedRenderCallback(l))
	l.Focus()
	return &threadsDashboard{
		com:     com,
		list:    l,
		cache:   cache,
		keyMap:  defaultThreadsKeyMap(),
		hovered: -1,
	}
}

// Fixed row counts for the chrome around the table. The detail pane is
// sized from its content (threadsDetailHeight), everything else is
// constant.
const (
	threadsTitleHeight   = 1
	threadsToolbarHeight = 1
	threadsTabsHeight    = 1
	threadsColumnsHeight = 1
	threadsRuleHeight    = 1
	threadsFooterHeight  = 1
	// threadsDetailMaxRows caps the detail pane so a long goal or error
	// can never crowd out the table it describes.
	threadsDetailMaxRows = 4
)

// chromeHeight is every row the screen spends on something other than the
// table body, for the current selection.
func (m *threadsDashboard) chromeHeight() int {
	h := threadsTitleHeight + threadsToolbarHeight + threadsTabsHeight +
		threadsColumnsHeight + threadsRuleHeight + threadsFooterHeight
	if d := m.detailHeight(); d > 0 {
		h += d + threadsRuleHeight
	}
	return h
}

// detailHeight is how many rows the detail pane needs right now: none when
// nothing is selected, otherwise one row per populated field.
func (m *threadsDashboard) detailHeight() int {
	sel := m.selected()
	if sel == nil {
		return 0
	}
	return min(threadsDetailMaxRows, len(threadsDetailLines(m.com.Styles, sel, m.width)))
}

// selected returns the thread under the list's selection, or nil when the
// (filtered) list is empty.
//
// Returns a pointer to a copy, not &m.visible[idx]: under filterAll,
// m.visible aliases the cache's backing array (see filterThreads), and
// threadListCache.applyEvent deletes from that array in place on a
// DeletedEvent, shifting elements under any pointer taken into it. No
// current caller holds the result across such a mutation, but returning
// an alias into shared, mutable storage is a structural landmine — copy
// out here instead of relying on every call site to stay careful.
func (m *threadsDashboard) selected() *proto.Thread {
	idx := m.list.Selected()
	if idx < 0 || idx >= len(m.visible) {
		return nil
	}
	t := m.visible[idx]
	return &t
}

// SetSize resizes the dashboard, leaving room for the chrome around the
// table.
func (m *threadsDashboard) SetSize(width, height int) {
	m.width = width
	m.height = height
	m.columns = computeThreadsColumns(max(0, width-2))
	m.list.SetSize(max(0, width-2), max(0, height-m.chromeHeight()))
	m.rebuildItems()
}

// SetActive sets whether the dashboard is showing. Becoming active
// immediately dispatches a refresh if the cache isn't fresh, so the list
// doesn't sit on stale data until the next TTL tick.
func (m *threadsDashboard) SetActive(active bool) tea.Cmd {
	m.active = active
	if !active {
		return nil
	}
	if m.cache.cache.Fresh(threadsCacheTTL) {
		return nil
	}
	return m.cache.dispatchRefresh(m.com)
}

// Draw renders the administration screen top to bottom: title bar with a
// Back button, toolbar, filter tabs, column header, table, an optional
// detail pane for the selection, and the help footer. Hit zones for every
// button and tab are rebuilt here, so a click always lands on what the
// last frame actually painted.
func (m *threadsDashboard) Draw(scr uv.Screen, area uv.Rectangle) {
	t := m.com.Styles
	m.zones = m.zones[:0]

	// Rows are frozen in the list memo once rendered, and the theme is
	// switched from the chat screen, which has no handle on the dashboard.
	// Noticing the palette build changed here is what keeps the table from
	// coming back in the previous theme's colors.
	if rev := t.Rev(); rev != m.styleRev {
		m.styleRev = rev
		m.list.InvalidateAll()
	}

	detailRows := m.detailHeight()
	sections := []layout.Constraint{
		layout.Len(threadsTitleHeight),
		layout.Len(threadsToolbarHeight),
		layout.Len(threadsTabsHeight),
		layout.Len(threadsColumnsHeight),
		layout.Fill(1),
	}
	var titleRect, toolbarRect, tabsRect, columnsRect, listRect uv.Rectangle
	var detailRuleRect, detailRect uv.Rectangle
	var ruleRect, footerRect uv.Rectangle
	if detailRows > 0 {
		sections = append(sections, layout.Len(threadsRuleHeight), layout.Len(detailRows))
	}
	sections = append(sections, layout.Len(threadsRuleHeight), layout.Len(threadsFooterHeight))

	targets := []*uv.Rectangle{&titleRect, &toolbarRect, &tabsRect, &columnsRect, &listRect}
	if detailRows > 0 {
		targets = append(targets, &detailRuleRect, &detailRect)
	}
	targets = append(targets, &ruleRect, &footerRect)
	layout.Vertical(sections...).Split(area).Assign(targets...)

	m.listRect = image.Rect(listRect.Min.X, listRect.Min.Y, listRect.Max.X, listRect.Max.Y)

	m.drawTitle(scr, titleRect)
	m.drawToolbar(scr, toolbarRect)
	m.drawTabs(scr, tabsRect)

	uv.NewStyledString(renderThreadsColumnHeader(t, m.columns, columnsRect.Dx()-2)).
		Draw(scr, indentRect(columnsRect))

	// The table body is indented to the same column as its header, so a
	// row's cells line up under the labels naming them.
	if len(m.visible) == 0 {
		uv.NewStyledString(t.Threads.Subtle.Render(m.emptyText())).Draw(scr, indentRect(listRect))
	} else {
		uv.NewStyledString(m.list.Render()).Draw(scr, indentRect(listRect))
	}

	if detailRows > 0 {
		m.drawRule(scr, detailRuleRect)
		lines := threadsDetailLines(t, m.selected(), detailRect.Dx()-2)
		row := detailRect.Min.Y
		for _, line := range lines[:min(len(lines), detailRows)] {
			lineRect := uv.Rectangle{
				Min: uv.Position{X: detailRect.Min.X + 1, Y: row},
				Max: uv.Position{X: detailRect.Max.X, Y: row + 1},
			}
			uv.NewStyledString(line).Draw(scr, lineRect)
			row++
		}
	}

	m.drawRule(scr, ruleRect)
	footer := shortHelpText(m.keyMap.ShortHelp(), footerRect.Dx()-2)
	uv.NewStyledString(t.Threads.Subtle.Render(footer)).Draw(scr, indentRect(footerRect))
}

// emptyText is the message shown in place of the table. It names the
// reason the table is empty — no threads at all, versus a filter hiding
// them — since the two need different next steps from the operator.
func (m *threadsDashboard) emptyText() string {
	if len(m.cache.cache.Value) == 0 {
		return "No threads yet — press n or click + New to start one."
	}
	return fmt.Sprintf("No %s threads — press a to show all.", strings.ToLower(m.filter.label()))
}

// indentRect insets a full-width row by one cell on each side, so text
// does not start hard against the terminal edge.
func indentRect(r uv.Rectangle) uv.Rectangle {
	r.Min.X = min(r.Min.X+1, r.Max.X)
	r.Max.X = max(r.Max.X-1, r.Min.X)
	return r
}

// drawRule paints a full-width horizontal separator.
func (m *threadsDashboard) drawRule(scr uv.Screen, rect uv.Rectangle) {
	if rect.Dx() <= 0 {
		return
	}
	line := strings.Repeat("─", rect.Dx())
	uv.NewStyledString(m.com.Styles.Threads.Rule.Render(line)).Draw(scr, rect)
}

// drawTitle paints the screen title, the total count, and the Back button
// right-aligned on the same row.
func (m *threadsDashboard) drawTitle(scr uv.Screen, rect uv.Rectangle) {
	t := m.com.Styles
	title := t.Threads.Title.Render("Threads")
	count := t.Threads.Subtle.Render(fmt.Sprintf("  %d total", len(m.cache.cache.Value)))
	uv.NewStyledString(title+count).Draw(scr, indentRect(rect))

	back := m.renderButton(actionBack, true)
	backWidth := ansi.StringWidth(back)
	if backWidth+2 > rect.Dx() {
		return
	}
	x := rect.Max.X - 1 - backWidth
	buttonRect := uv.Rectangle{
		Min: uv.Position{X: x, Y: rect.Min.Y},
		Max: uv.Position{X: rect.Max.X - 1, Y: rect.Min.Y + 1},
	}
	uv.NewStyledString(back).Draw(scr, buttonRect)
	m.zones = append(m.zones, threadsHitZone{
		rect:    image.Rect(buttonRect.Min.X, buttonRect.Min.Y, buttonRect.Max.X, buttonRect.Max.Y),
		action:  actionBack,
		enabled: true,
	})
}

// drawToolbar paints the action buttons left to right, registering a hit
// zone for each. Disabled buttons stay in place rather than disappearing:
// a toolbar whose buttons move as the selection changes is one you have to
// re-read every time.
func (m *threadsDashboard) drawToolbar(scr uv.Screen, rect uv.Rectangle) {
	sel := m.selected()
	x := rect.Min.X + 1
	for _, action := range threadsToolbarActions {
		enabled := action.enabledFor(sel)
		label := m.renderButton(action, enabled)
		w := ansi.StringWidth(label)
		if x+w > rect.Max.X {
			return
		}
		buttonRect := uv.Rectangle{
			Min: uv.Position{X: x, Y: rect.Min.Y},
			Max: uv.Position{X: x + w, Y: rect.Min.Y + 1},
		}
		uv.NewStyledString(label).Draw(scr, buttonRect)
		m.zones = append(m.zones, threadsHitZone{
			rect:    image.Rect(buttonRect.Min.X, buttonRect.Min.Y, buttonRect.Max.X, buttonRect.Max.Y),
			action:  action,
			enabled: enabled,
		})
		x += w + 1
	}
}

// renderButton styles one button for its current state. Hover is resolved
// against the zone list from the previous frame, which is what the mouse
// handler wrote into m.hovered.
func (m *threadsDashboard) renderButton(action threadAction, enabled bool) string {
	t := m.com.Styles
	style := t.Threads.ButtonIdle
	switch {
	case !enabled:
		style = t.Threads.ButtonDisabled
	case m.isHovered(action):
		style = t.Threads.ButtonHover
		if action.destructive() {
			style = t.Threads.ButtonDanger
		}
	}
	return style.Render(action.label())
}

// isHovered reports whether the pointer is currently over the button for
// action.
func (m *threadsDashboard) isHovered(action threadAction) bool {
	if m.hovered < 0 || m.hovered >= len(m.zones) {
		return false
	}
	z := m.zones[m.hovered]
	return !z.isFilter && z.action == action
}

// drawTabs paints the status filter tabs with their counts.
func (m *threadsDashboard) drawTabs(scr uv.Screen, rect uv.Rectangle) {
	t := m.com.Styles
	all := m.cache.cache.Value
	x := rect.Min.X + 1
	for _, f := range threadsFilters {
		style := t.Threads.TabInactive
		if f == m.filter {
			style = t.Threads.TabActive
		}
		label := style.Render(fmt.Sprintf("%s %d", f.label(), len(filterThreads(all, f))))
		w := ansi.StringWidth(label)
		if x+w > rect.Max.X {
			return
		}
		tabRect := uv.Rectangle{
			Min: uv.Position{X: x, Y: rect.Min.Y},
			Max: uv.Position{X: x + w, Y: rect.Min.Y + 1},
		}
		uv.NewStyledString(label).Draw(scr, tabRect)
		m.zones = append(m.zones, threadsHitZone{
			rect:     image.Rect(tabRect.Min.X, tabRect.Min.Y, tabRect.Max.X, tabRect.Max.Y),
			filter:   f,
			isFilter: true,
			enabled:  true,
		})
		x += w + 1
	}
}

// shortHelpText joins a set of key bindings into a single-line help
// string ("key desc  •  key desc  ..."), truncated to width.
func shortHelpText(bindings []key.Binding, width int) string {
	parts := make([]string, 0, len(bindings))
	for _, b := range bindings {
		if !b.Enabled() {
			continue
		}
		h := b.Help()
		parts = append(parts, h.Key+" "+h.Desc)
	}
	return ansi.Truncate(strings.Join(parts, "  •  "), width, "…")
}

// ApplyThreadsLoaded writes through an off-thread thread list fetch and
// rebuilds the list items to reflect it.
func (m *threadsDashboard) ApplyThreadsLoaded(msg threadsLoadedMsg) []tea.Cmd {
	cmds, _ := m.cache.applyLoaded(m.com, msg)
	m.rebuildItems()
	return cmds
}

// ApplyThreadEvent reacts to a thread pubsub event: it write-throughs the
// event into the cache, rebuilds the list, and — since applyThreadEvent
// always invalidates the TTL — re-arms a refresh immediately while the
// dashboard is active so the optimistic update is reconciled promptly.
func (m *threadsDashboard) ApplyThreadEvent(evt pubsub.Event[proto.Thread]) tea.Cmd {
	m.cache.applyEvent(evt)
	m.rebuildItems()
	if !m.active {
		return nil
	}
	return m.cache.dispatchRefresh(m.com)
}

// Refresh dispatches an off-thread thread list re-fetch, bypassing the TTL.
// Exposed for the router (root.go) to call after an action (merge/remove)
// that changes thread state out of band, without reaching into the
// unexported cache field itself.
func (m *threadsDashboard) Refresh() tea.Cmd {
	return m.cache.dispatchRefresh(m.com)
}

// Tick is the TTL backstop, mirroring UI.staleWorkspaceRefreshCmds: called
// at the tail of Update, it schedules an off-thread re-probe if the cache
// has gone stale while the dashboard is active.
func (m *threadsDashboard) Tick(active bool, com *common.Common) tea.Cmd {
	return m.cache.staleRefreshCmd(com, active)
}

// rebuildItems applies the active filter to the cache and converts the
// surviving threads into list items. The selection is kept on the same
// thread across rebuilds where possible — a list that jumps back to the
// top every time a status event lands is unusable while anything is
// running.
func (m *threadsDashboard) rebuildItems() {
	var selectedID string
	if sel := m.selected(); sel != nil {
		selectedID = sel.ID
	}

	m.visible = filterThreads(m.cache.cache.Value, m.filter)
	items := make([]list.Item, len(m.visible))
	for i, s := range m.visible {
		items[i] = newThreadItem(m.com.Styles, s, m.columns)
	}
	m.list.SetItems(items...)

	for i, s := range m.visible {
		if selectedID != "" && s.ID == selectedID {
			m.list.SetSelected(i)
			return
		}
	}

	// Nothing to restore: land on the first row. A table whose toolbar is
	// entirely disabled because no row is selected is a dead screen, and
	// the list also renders nothing until its window is placed (SetItems
	// alone leaves it unset).
	if len(m.visible) > 0 {
		m.list.SetSelected(0)
		m.list.ScrollToTop()
	}
}

// setFilter switches the visible status class and rebuilds the table.
func (m *threadsDashboard) setFilter(f threadsFilter) {
	if m.filter == f {
		return
	}
	m.filter = f
	m.rebuildItems()
	m.list.SetSelected(0)
	m.list.ScrollToTop()
	// The detail pane appears or disappears with the selection, so the
	// table's height can change with the filter.
	m.list.SetSize(max(0, m.width-2), max(0, m.height-m.chromeHeight()))
}

// HandleMouseMotion updates hover feedback for the toolbar and tabs.
func (m *threadsDashboard) HandleMouseMotion(msg tea.MouseMotionMsg) {
	pt := image.Pt(msg.X, msg.Y)
	m.hovered = -1
	for i, z := range m.zones {
		if pt.In(z.rect) {
			m.hovered = i
			return
		}
	}
}

// HandleMouseClick routes a click to a button, a tab, or a table row.
// Clicking a row only selects it — the action buttons are how a click
// acts, so a mis-aimed click can never merge or remove anything.
func (m *threadsDashboard) HandleMouseClick(msg tea.MouseClickMsg) (handled bool, cmd tea.Cmd) {
	if msg.Button != tea.MouseLeft {
		return false, nil
	}
	pt := image.Pt(msg.X, msg.Y)

	for _, z := range m.zones {
		if !pt.In(z.rect) {
			continue
		}
		switch {
		case z.isFilter:
			m.setFilter(z.filter)
			return true, nil
		case !z.enabled:
			// A disabled button still swallows the click: falling through
			// to the row hit-test would move the selection under the
			// pointer, which is not what clicking a dimmed button means.
			return true, nil
		default:
			return true, m.runAction(z.action)
		}
	}

	if idx, ok := m.rowAt(pt); ok {
		m.list.SetSelected(idx)
		m.list.SetSize(max(0, m.width-2), max(0, m.height-m.chromeHeight()))
		return true, nil
	}
	return false, nil
}

// HandleMouseWheel scrolls the table.
func (m *threadsDashboard) HandleMouseWheel(msg tea.MouseWheelMsg) bool {
	if !image.Pt(msg.X, msg.Y).In(m.listRect) {
		return false
	}
	switch msg.Button {
	case tea.MouseWheelUp:
		m.list.ScrollBy(-3)
	case tea.MouseWheelDown:
		m.list.ScrollBy(3)
	default:
		return false
	}
	return true
}

// rowAt maps a point inside the table body onto a thread index. Rows are
// exactly one line tall with no gap, so the index is the scroll offset
// plus the row's distance from the top of the body.
func (m *threadsDashboard) rowAt(pt image.Point) (int, bool) {
	if !pt.In(m.listRect) || len(m.visible) == 0 {
		return 0, false
	}
	idx := m.list.Offset() + (pt.Y - m.listRect.Min.Y)
	if idx < 0 || idx >= len(m.visible) {
		return 0, false
	}
	return idx, true
}

// runAction performs a toolbar action against the current selection. Key
// bindings route here too, so a shortcut and its button can never drift.
func (m *threadsDashboard) runAction(action threadAction) tea.Cmd {
	sel := m.selected()
	if !action.enabledFor(sel) {
		return nil
	}
	switch action {
	case actionNew:
		return func() tea.Msg { return openThreadCreateMsg{} }
	case actionOpen:
		id, sessionID, name := sel.ID, sel.SessionID, sel.Name
		return func() tea.Msg { return enterThreadMsg{id: id, sessionID: sessionID, name: name} }
	case actionMerge:
		id := sel.ID
		return func() tea.Msg { return mergeThreadMsg{id: id} }
	case actionCancel:
		id, kind := sel.ID, sel.Kind
		return func() tea.Msg { return cancelDelegationMsg{id: id, kind: kind} }
	case actionRemove:
		id, name := sel.ID, sel.Name
		return func() tea.Msg { return confirmRemoveThreadMsg{id: id, name: name} }
	case actionRefresh:
		return m.cache.dispatchRefresh(m.com)
	case actionBack:
		return func() tea.Msg { return leaveDashboardMsg{} }
	default:
		return nil
	}
}

// HandleKey handles a key press while the dashboard has focus. Every
// action key goes through runAction, the same entry point the toolbar
// buttons use, so the two can never disagree about what is allowed.
func (m *threadsDashboard) HandleKey(msg tea.KeyPressMsg) (handled bool, cmd tea.Cmd) {
	switch {
	case key.Matches(msg, m.keyMap.Up):
		m.list.SelectPrev()
		m.list.SetSize(max(0, m.width-2), max(0, m.height-m.chromeHeight()))
		return true, nil
	case key.Matches(msg, m.keyMap.Down):
		m.list.SelectNext()
		m.list.SetSize(max(0, m.width-2), max(0, m.height-m.chromeHeight()))
		return true, nil
	case key.Matches(msg, m.keyMap.Enter):
		return true, m.runAction(actionOpen)
	case key.Matches(msg, m.keyMap.New):
		return true, m.runAction(actionNew)
	case key.Matches(msg, m.keyMap.Merge):
		return true, m.runAction(actionMerge)
	case key.Matches(msg, m.keyMap.Remove):
		return true, m.runAction(actionRemove)
	case key.Matches(msg, m.keyMap.Cancel):
		return true, m.runAction(actionCancel)
	case key.Matches(msg, m.keyMap.Reload):
		return true, m.runAction(actionRefresh)
	case key.Matches(msg, m.keyMap.NextFilter):
		m.setFilter(nextThreadsFilter(m.filter, 1))
		return true, nil
	case key.Matches(msg, m.keyMap.AllFilter):
		m.setFilter(filterAll)
		return true, nil
	}
	return false, nil
}

// nextThreadsFilter steps the tab selection by delta, wrapping around.
func nextThreadsFilter(current threadsFilter, delta int) threadsFilter {
	for i, f := range threadsFilters {
		if f != current {
			continue
		}
		next := (i + delta + len(threadsFilters)) % len(threadsFilters)
		return threadsFilters[next]
	}
	return filterAll
}

// threadMergeable reports whether a delegation in the given kind/status is
// eligible to be merged: a task has no worktree/branch of its own (see
// TaskController's doc comment), so it never reports mergeable regardless
// of status; a thread is mergeable in any status other than already merged
// or currently merging.
func threadMergeable(kind, status string) bool {
	if proto.ThreadKind(kind) == proto.ThreadKindTask {
		return false
	}
	switch proto.ThreadStatus(status) {
	case proto.ThreadStatusMerged, proto.ThreadStatusMerging:
		return false
	default:
		return true
	}
}

// threadItem renders a single row of the threads table: name, status,
// branch, relative last-activity time, and goal, each in its own fixed
// column so the table can be read down a column instead of parsed line by
// line.
type threadItem struct {
	*list.Versioned
	thread  proto.Thread
	sty     *styles.Styles
	columns threadsColumns
	focused bool
}

var _ list.Item = (*threadItem)(nil)

// newThreadItem creates a new threadItem for s under the given column
// layout.
func newThreadItem(sty *styles.Styles, s proto.Thread, columns threadsColumns) *threadItem {
	return &threadItem{
		Versioned: list.NewVersioned(),
		thread:    s,
		sty:       sty,
		columns:   columns,
	}
}

// SetFocused implements list.Focusable.
func (it *threadItem) SetFocused(focused bool) {
	if it.focused == focused {
		return
	}
	it.focused = focused
	it.Bump()
}

// Finished implements list.Item. Thread rows are render-stable outside of
// SetFocused (which bumps the version and invalidates the frozen entry) —
// the dashboard rebuilds items wholesale on every cache change instead of
// mutating them in place.
func (it *threadItem) Finished() bool {
	return true
}

// Render implements list.Item, painting one table row into the column
// layout. Cells are padded to their column width rather than joined with
// separators, so every field starts at the same x on every row — the
// point of a table over a list. Only the status cell is colored; the rest
// of the row stays neutral so the color means "state", not "row".
//
// A task has no branch of its own (see TaskController's doc comment); its
// branch cell is simply blank, which keeps the columns aligned where the
// old separator-joined line would have collapsed them.
func (it *threadItem) Render(width int) string {
	style := it.sty.Threads.RowBase
	if it.focused {
		style = it.sty.Threads.RowSelected
	}
	lineWidth := max(0, width-style.GetHorizontalFrameSize())

	s := it.thread
	c := it.columns

	var b strings.Builder
	b.WriteString(presentation.PadTo(s.Name, c.name))
	b.WriteString("  ")
	statusCell := presentation.PadTo(strings.ToUpper(s.Status), c.status)
	b.WriteString(threadStatusStyle(it.sty, s.Status).Render(statusCell))
	if c.branch > 0 {
		b.WriteString("  " + presentation.PadTo(s.Branch, c.branch))
	}
	if c.updated > 0 {
		b.WriteString("  " + presentation.PadTo(humanize.Time(time.Unix(s.UpdatedAt, 0)), c.updated))
	}
	if c.goal > 0 {
		b.WriteString("  " + presentation.PadTo(s.Goal, c.goal))
	}

	return style.Render(ansi.Truncate(b.String(), lineWidth, "…"))
}
