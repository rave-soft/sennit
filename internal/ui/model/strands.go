package model

// strandsDashboard is the strands list view (added in a later step to the
// main router). It wraps a list.List over the memoized strand cache
// (strands_cache.go) the same way Chat wraps a list.List over messages:
// items are rebuilt from the cache on load/event, never fetched directly
// from Draw/HandleKey.
//
// Status badges reuse the existing Status.*Message semantic styles rather
// than a live per-row spinner: an anim.Anim only advances when something
// drives it with anim.StepMsg on a ticker (see chat/assistant.go, driven by
// Chat.Animate from the main Update loop), and this dashboard has no such
// driver wired in yet — that lands in step 4 alongside the router. Wiring
// an anim here now would render a permanently frozen first frame, which is
// worse than a static badge.

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/ultraviolet/layout"
	"github.com/charmbracelet/x/ansi"
	"github.com/dustin/go-humanize"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/strand"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/list"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// enterStrandMsg requests attaching to a strand's own workspace/session.
// Consumed by the router (root.go).
type enterStrandMsg struct {
	id        string
	sessionID string
}

// openStrandCreateMsg requests opening the create-strand dialog. The dialog
// itself is implemented in a later step; this is a placeholder message so
// the dashboard can be wired up ahead of it.
type openStrandCreateMsg struct{}

// mergeStrandMsg requests merging a completed strand's branch back into its
// base branch.
type mergeStrandMsg struct {
	id string
}

// removeStrandMsg requests removing a strand. Sent directly on 'x' for now
// — no confirmation dialog yet.
type removeStrandMsg struct {
	id string
}

// strandsKeyMap holds the key bindings local to the strands dashboard. It
// is intentionally separate from the app-wide KeyMap in keys.go: these
// bindings only apply while the dashboard has focus.
type strandsKeyMap struct {
	Up     key.Binding
	Down   key.Binding
	Enter  key.Binding
	New    key.Binding
	Merge  key.Binding
	Remove key.Binding
	Reload key.Binding
}

// defaultStrandsKeyMap returns the strands dashboard's key bindings.
func defaultStrandsKeyMap() strandsKeyMap {
	return strandsKeyMap{
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
			key.WithKeys("x"),
			key.WithHelp("x", "remove"),
		),
		Reload: key.NewBinding(
			key.WithKeys("r"),
			key.WithHelp("r", "refresh"),
		),
	}
}

// ShortHelp implements [help.KeyMap] so the footer can be built with the
// same shared help machinery other views use.
func (k strandsKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Up, k.Down, k.Enter, k.New, k.Merge, k.Remove, k.Reload}
}

// strandsDashboard renders the strand list: a header line with the strand
// count, the scrollable list itself (or an empty-state message), and a
// footer help line.
type strandsDashboard struct {
	com    *common.Common
	list   *list.List
	cache  strandsCacheState
	keyMap strandsKeyMap

	width, height int
	active        bool
}

// newStrandsDashboard creates a new strands dashboard.
func newStrandsDashboard(com *common.Common) *strandsDashboard {
	l := list.NewList()
	l.RegisterRenderCallback(list.FocusedRenderCallback(l))
	l.Focus()
	return &strandsDashboard{
		com:    com,
		list:   l,
		keyMap: defaultStrandsKeyMap(),
	}
}

// strandsHeaderHeight and strandsFooterHeight are the fixed rows reserved
// around the list for the count header and the help footer.
const (
	strandsHeaderHeight = 1
	strandsFooterHeight = 1
)

// SetSize resizes the dashboard, leaving room for the header and footer
// lines around the list.
func (m *strandsDashboard) SetSize(width, height int) {
	m.width = width
	m.height = height
	listHeight := max(0, height-strandsHeaderHeight-strandsFooterHeight)
	m.list.SetSize(width, listHeight)
}

// SetActive sets whether the dashboard is showing. Becoming active
// immediately dispatches a refresh if the cache isn't fresh, so the list
// doesn't sit on stale data until the next TTL tick.
func (m *strandsDashboard) SetActive(active bool) tea.Cmd {
	m.active = active
	if !active {
		return nil
	}
	if m.cache.fresh(strandsCacheTTL) {
		return nil
	}
	return m.cache.dispatchStrandsRefresh(m.com)
}

// Draw renders the dashboard into area: header, list (or empty state), then
// footer, following the header+content+footer layout in sidebar.go and the
// uv.NewStyledString(str).Draw(scr, rect) convention from chat.go.
func (m *strandsDashboard) Draw(scr uv.Screen, area uv.Rectangle) {
	t := m.com.Styles

	var headerRect, listRect, footerRect uv.Rectangle
	layout.Vertical(
		layout.Len(strandsHeaderHeight),
		layout.Fill(1),
		layout.Len(strandsFooterHeight),
	).Split(area).Assign(&headerRect, &listRect, &footerRect)

	header := fmt.Sprintf("Strands (%d)", len(m.cache.strands))
	uv.NewStyledString(t.Status.Help.Render(header)).Draw(scr, headerRect)

	if len(m.cache.strands) == 0 {
		empty := "No strands yet — press n to create one."
		uv.NewStyledString(t.Status.Help.Render(empty)).Draw(scr, listRect)
	} else {
		uv.NewStyledString(m.list.Render()).Draw(scr, listRect)
	}

	footer := shortHelpText(m.keyMap.ShortHelp(), footerRect.Dx())
	uv.NewStyledString(t.Status.Help.Render(footer)).Draw(scr, footerRect)
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

// ApplyStrandsLoaded writes through an off-thread strand list fetch and
// rebuilds the list items to reflect it.
func (m *strandsDashboard) ApplyStrandsLoaded(msg strandsLoadedMsg) []tea.Cmd {
	cmds := m.cache.applyStrandsLoaded(m.com, msg)
	m.rebuildItems()
	return cmds
}

// ApplyStrandEvent reacts to a strand pubsub event: it write-throughs the
// event into the cache, rebuilds the list, and — since applyStrandEvent
// always invalidates the TTL — re-arms a refresh immediately while the
// dashboard is active so the optimistic update is reconciled promptly.
func (m *strandsDashboard) ApplyStrandEvent(evt pubsub.Event[proto.Strand]) tea.Cmd {
	m.cache.applyStrandEvent(evt)
	m.rebuildItems()
	if !m.active {
		return nil
	}
	return m.cache.dispatchStrandsRefresh(m.com)
}

// Refresh dispatches an off-thread strand list re-fetch, bypassing the TTL.
// Exposed for the router (root.go) to call after an action (merge/remove)
// that changes strand state out of band, without reaching into the
// unexported cache field itself.
func (m *strandsDashboard) Refresh() tea.Cmd {
	return m.cache.dispatchStrandsRefresh(m.com)
}

// Tick is the TTL backstop, mirroring UI.staleWorkspaceRefreshCmds: called
// at the tail of Update, it schedules an off-thread re-probe if the cache
// has gone stale while the dashboard is active.
func (m *strandsDashboard) Tick(active bool, com *common.Common) tea.Cmd {
	return m.cache.staleStrandsRefreshCmd(com, active)
}

// rebuildItems converts the cached strand list into list.Items and applies
// them to the wrapped list.
func (m *strandsDashboard) rebuildItems() {
	items := make([]list.Item, len(m.cache.strands))
	for i, s := range m.cache.strands {
		items[i] = newStrandItem(m.com.Styles, s)
	}
	m.list.SetItems(items...)
}

// HandleKey handles a key press while the dashboard has focus.
func (m *strandsDashboard) HandleKey(msg tea.KeyPressMsg) (handled bool, cmd tea.Cmd) {
	switch {
	case key.Matches(msg, m.keyMap.Up):
		m.list.SelectPrev()
		return true, nil
	case key.Matches(msg, m.keyMap.Down):
		m.list.SelectNext()
		return true, nil
	case key.Matches(msg, m.keyMap.Enter):
		item, ok := m.list.SelectedItem().(*strandItem)
		if !ok {
			return true, nil
		}
		id, sessionID := item.strand.ID, item.strand.SessionID
		return true, func() tea.Msg { return enterStrandMsg{id: id, sessionID: sessionID} }
	case key.Matches(msg, m.keyMap.New):
		return true, func() tea.Msg { return openStrandCreateMsg{} }
	case key.Matches(msg, m.keyMap.Merge):
		item, ok := m.list.SelectedItem().(*strandItem)
		if !ok || !strandMergeable(item.strand.Status) {
			return true, nil
		}
		id := item.strand.ID
		return true, func() tea.Msg { return mergeStrandMsg{id: id} }
	case key.Matches(msg, m.keyMap.Remove):
		item, ok := m.list.SelectedItem().(*strandItem)
		if !ok {
			return true, nil
		}
		id := item.strand.ID
		// TODO: confirm before removing — a confirmation dialog is planned
		// for a later step. For now this fires immediately.
		return true, func() tea.Msg { return removeStrandMsg{id: id} }
	case key.Matches(msg, m.keyMap.Reload):
		return true, m.cache.dispatchStrandsRefresh(m.com)
	}
	return false, nil
}

// strandMergeable reports whether a strand in the given status is eligible
// to be merged: anything other than already merged or currently merging.
func strandMergeable(status string) bool {
	switch strand.Status(status) {
	case strand.StatusMerged, strand.StatusMerging:
		return false
	default:
		return true
	}
}

// strandItem renders a single row of the strands dashboard: name, status
// badge, branch, relative last-activity time, and goal (truncated to fit).
type strandItem struct {
	*list.Versioned
	strand  proto.Strand
	sty     *styles.Styles
	focused bool
}

var _ list.Item = (*strandItem)(nil)

// newStrandItem creates a new strandItem for s.
func newStrandItem(sty *styles.Styles, s proto.Strand) *strandItem {
	return &strandItem{
		Versioned: list.NewVersioned(),
		strand:    s,
		sty:       sty,
	}
}

// SetFocused implements list.Focusable.
func (it *strandItem) SetFocused(focused bool) {
	if it.focused == focused {
		return
	}
	it.focused = focused
	it.Bump()
}

// Finished implements list.Item. Strand rows are render-stable outside of
// SetFocused (which bumps the version and invalidates the frozen entry) —
// the dashboard rebuilds items wholesale on every cache change instead of
// mutating them in place.
func (it *strandItem) Finished() bool {
	return true
}

// strandBadge renders a status badge reusing the existing semantic
// Status.*Message styles (styles.go) rather than inventing new colors.
func strandBadge(sty *styles.Styles, status string) string {
	label := strings.ToUpper(status)
	var style = sty.Status.InfoMessage
	switch strand.Status(status) {
	case strand.StatusCompleted, strand.StatusMerged:
		style = sty.Status.SuccessMessage
	case strand.StatusMerging, strand.StatusInterrupted:
		style = sty.Status.WarnMessage
	case strand.StatusConflict, strand.StatusMergeBlocked, strand.StatusFailed:
		style = sty.Status.ErrorMessage
	}
	return style.Render(label)
}

// Render implements list.Item. The row is: name • status badge • branch •
// relative last-activity time • goal — with the goal truncated to whatever
// width remains after the fixed-form fields.
func (it *strandItem) Render(width int) string {
	style := it.sty.Dialog.NormalItem
	if it.focused {
		style = it.sty.Dialog.SelectedItem
	}
	// Content must fit within width minus the style's own padding, or the
	// padding pushes the rendered line past width (see sessions_item.go's
	// renderItem for the same accounting).
	lineWidth := max(0, width-style.GetHorizontalFrameSize())

	s := it.strand
	badge := strandBadge(it.sty, s.Status)
	ago := humanize.Time(time.Unix(s.UpdatedAt, 0))

	fixed := fmt.Sprintf("%s  %s  %s  %s", s.Name, badge, s.Branch, ago)
	fixedWidth := ansi.StringWidth(fixed)

	line := fixed
	if remaining := lineWidth - fixedWidth - 2; remaining > 0 && s.Goal != "" {
		line += "  " + ansi.Truncate(s.Goal, remaining, "…")
	}

	return style.Render(ansi.Truncate(line, lineWidth, "…"))
}
