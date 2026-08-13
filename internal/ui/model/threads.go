package model

// threadsDashboard is the threads list view (added in a later step to the
// main router). It wraps a list.List over the memoized thread cache
// (threads_cache.go) the same way Chat wraps a list.List over messages:
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
	"github.com/rave-soft/braid/internal/thread"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/list"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// enterThreadMsg requests attaching to a thread's own workspace/session.
// Consumed by the router (root.go).
type enterThreadMsg struct {
	id        string
	sessionID string
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

// threadsKeyMap holds the key bindings local to the threads dashboard. It
// is intentionally separate from the app-wide KeyMap in keys.go: these
// bindings only apply while the dashboard has focus.
type threadsKeyMap struct {
	Up     key.Binding
	Down   key.Binding
	Enter  key.Binding
	New    key.Binding
	Merge  key.Binding
	Remove key.Binding
	Reload key.Binding
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
		Reload: key.NewBinding(
			key.WithKeys("r"),
			key.WithHelp("r", "refresh"),
		),
	}
}

// ShortHelp implements [help.KeyMap] so the footer can be built with the
// same shared help machinery other views use.
func (k threadsKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Up, k.Down, k.Enter, k.New, k.Merge, k.Remove, k.Reload}
}

// threadsDashboard renders the thread list: a header line with the thread
// count, the scrollable list itself (or an empty-state message), and a
// footer help line.
type threadsDashboard struct {
	com    *common.Common
	list   *list.List
	cache  threadsCacheState
	keyMap threadsKeyMap

	width, height int
	active        bool
}

// newThreadsDashboard creates a new threads dashboard.
func newThreadsDashboard(com *common.Common) *threadsDashboard {
	l := list.NewList()
	l.RegisterRenderCallback(list.FocusedRenderCallback(l))
	l.Focus()
	return &threadsDashboard{
		com:    com,
		list:   l,
		keyMap: defaultThreadsKeyMap(),
	}
}

// threadsHeaderHeight and threadsFooterHeight are the fixed rows reserved
// around the list for the count header and the help footer.
const (
	threadsHeaderHeight = 1
	threadsFooterHeight = 1
)

// SetSize resizes the dashboard, leaving room for the header and footer
// lines around the list.
func (m *threadsDashboard) SetSize(width, height int) {
	m.width = width
	m.height = height
	listHeight := max(0, height-threadsHeaderHeight-threadsFooterHeight)
	m.list.SetSize(width, listHeight)
}

// SetActive sets whether the dashboard is showing. Becoming active
// immediately dispatches a refresh if the cache isn't fresh, so the list
// doesn't sit on stale data until the next TTL tick.
func (m *threadsDashboard) SetActive(active bool) tea.Cmd {
	m.active = active
	if !active {
		return nil
	}
	if m.cache.cache.fresh(threadsCacheTTL) {
		return nil
	}
	return m.cache.dispatchThreadsRefresh(m.com)
}

// Draw renders the dashboard into area: header, list (or empty state), then
// footer, following the header+content+footer layout in sidebar.go and the
// uv.NewStyledString(str).Draw(scr, rect) convention from chat.go.
func (m *threadsDashboard) Draw(scr uv.Screen, area uv.Rectangle) {
	t := m.com.Styles

	var headerRect, listRect, footerRect uv.Rectangle
	layout.Vertical(
		layout.Len(threadsHeaderHeight),
		layout.Fill(1),
		layout.Len(threadsFooterHeight),
	).Split(area).Assign(&headerRect, &listRect, &footerRect)

	header := fmt.Sprintf("Threads (%d)", len(m.cache.cache.value))
	uv.NewStyledString(t.Status.Help.Render(header)).Draw(scr, headerRect)

	if len(m.cache.cache.value) == 0 {
		empty := "No threads yet — press n to create one."
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

// ApplyThreadsLoaded writes through an off-thread thread list fetch and
// rebuilds the list items to reflect it.
func (m *threadsDashboard) ApplyThreadsLoaded(msg threadsLoadedMsg) []tea.Cmd {
	cmds := m.cache.applyThreadsLoaded(m.com, msg)
	m.rebuildItems()
	return cmds
}

// ApplyThreadEvent reacts to a thread pubsub event: it write-throughs the
// event into the cache, rebuilds the list, and — since applyThreadEvent
// always invalidates the TTL — re-arms a refresh immediately while the
// dashboard is active so the optimistic update is reconciled promptly.
func (m *threadsDashboard) ApplyThreadEvent(evt pubsub.Event[proto.Thread]) tea.Cmd {
	m.cache.applyThreadEvent(evt)
	m.rebuildItems()
	if !m.active {
		return nil
	}
	return m.cache.dispatchThreadsRefresh(m.com)
}

// Refresh dispatches an off-thread thread list re-fetch, bypassing the TTL.
// Exposed for the router (root.go) to call after an action (merge/remove)
// that changes thread state out of band, without reaching into the
// unexported cache field itself.
func (m *threadsDashboard) Refresh() tea.Cmd {
	return m.cache.dispatchThreadsRefresh(m.com)
}

// Tick is the TTL backstop, mirroring UI.staleWorkspaceRefreshCmds: called
// at the tail of Update, it schedules an off-thread re-probe if the cache
// has gone stale while the dashboard is active.
func (m *threadsDashboard) Tick(active bool, com *common.Common) tea.Cmd {
	return m.cache.staleThreadsRefreshCmd(com, active)
}

// rebuildItems converts the cached thread list into list.Items and applies
// them to the wrapped list.
func (m *threadsDashboard) rebuildItems() {
	items := make([]list.Item, len(m.cache.cache.value))
	for i, s := range m.cache.cache.value {
		items[i] = newThreadItem(m.com.Styles, s)
	}
	m.list.SetItems(items...)
}

// HandleKey handles a key press while the dashboard has focus.
func (m *threadsDashboard) HandleKey(msg tea.KeyPressMsg) (handled bool, cmd tea.Cmd) {
	switch {
	case key.Matches(msg, m.keyMap.Up):
		m.list.SelectPrev()
		return true, nil
	case key.Matches(msg, m.keyMap.Down):
		m.list.SelectNext()
		return true, nil
	case key.Matches(msg, m.keyMap.Enter):
		item, ok := m.list.SelectedItem().(*threadItem)
		if !ok {
			return true, nil
		}
		id, sessionID := item.thread.ID, item.thread.SessionID
		return true, func() tea.Msg { return enterThreadMsg{id: id, sessionID: sessionID} }
	case key.Matches(msg, m.keyMap.New):
		return true, func() tea.Msg { return openThreadCreateMsg{} }
	case key.Matches(msg, m.keyMap.Merge):
		item, ok := m.list.SelectedItem().(*threadItem)
		if !ok || !threadMergeable(item.thread.Status) {
			return true, nil
		}
		id := item.thread.ID
		return true, func() tea.Msg { return mergeThreadMsg{id: id} }
	case key.Matches(msg, m.keyMap.Remove):
		item, ok := m.list.SelectedItem().(*threadItem)
		if !ok {
			return true, nil
		}
		id, name := item.thread.ID, item.thread.Name
		return true, func() tea.Msg { return confirmRemoveThreadMsg{id: id, name: name} }
	case key.Matches(msg, m.keyMap.Reload):
		return true, m.cache.dispatchThreadsRefresh(m.com)
	}
	return false, nil
}

// threadMergeable reports whether a thread in the given status is eligible
// to be merged: anything other than already merged or currently merging.
func threadMergeable(status string) bool {
	switch thread.Status(status) {
	case thread.StatusMerged, thread.StatusMerging:
		return false
	default:
		return true
	}
}

// threadItem renders a single row of the threads dashboard: name, status
// badge, branch, relative last-activity time, and goal (truncated to fit).
type threadItem struct {
	*list.Versioned
	thread  proto.Thread
	sty     *styles.Styles
	focused bool
}

var _ list.Item = (*threadItem)(nil)

// newThreadItem creates a new threadItem for s.
func newThreadItem(sty *styles.Styles, s proto.Thread) *threadItem {
	return &threadItem{
		Versioned: list.NewVersioned(),
		thread:    s,
		sty:       sty,
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

// threadBadge renders a status badge reusing the existing semantic
// Status.*Message styles (styles.go) rather than inventing new colors.
func threadBadge(sty *styles.Styles, status string) string {
	label := strings.ToUpper(status)
	style := sty.Status.InfoMessage
	switch thread.Status(status) {
	case thread.StatusCompleted, thread.StatusMerged:
		style = sty.Status.SuccessMessage
	case thread.StatusMerging, thread.StatusInterrupted:
		style = sty.Status.WarnMessage
	case thread.StatusConflict, thread.StatusMergeBlocked, thread.StatusFailed:
		style = sty.Status.ErrorMessage
	}
	return style.Render(label)
}

// Render implements list.Item. The row is: name • status badge • branch •
// relative last-activity time • goal — with the goal truncated to whatever
// width remains after the fixed-form fields.
func (it *threadItem) Render(width int) string {
	style := it.sty.Dialog.NormalItem
	if it.focused {
		style = it.sty.Dialog.SelectedItem
	}
	// Content must fit within width minus the style's own padding, or the
	// padding pushes the rendered line past width (see sessions_item.go's
	// renderItem for the same accounting).
	lineWidth := max(0, width-style.GetHorizontalFrameSize())

	s := it.thread
	badge := threadBadge(it.sty, s.Status)
	ago := humanize.Time(time.Unix(s.UpdatedAt, 0))

	fixed := fmt.Sprintf("%s  %s  %s  %s", s.Name, badge, s.Branch, ago)
	fixedWidth := ansi.StringWidth(fixed)

	line := fixed
	if remaining := lineWidth - fixedWidth - 2; remaining > 0 && s.Goal != "" {
		line += "  " + ansi.Truncate(s.Goal, remaining, "…")
	}

	return style.Render(ansi.Truncate(line, lineWidth, "…"))
}
