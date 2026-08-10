package model

import (
	"image"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/clipperhouse/displaywidth"
	"github.com/clipperhouse/uax29/v2/words"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/ui/anim"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/list"
)

// Constants for multi-click detection.
const (
	doubleClickThreshold = 400 * time.Millisecond // 0.4s is typical double-click threshold
	clickTolerance       = 2                      // x,y tolerance for double/tripple click
)

// DelayedClickMsg is sent after the double-click threshold to trigger a
// single-click action (like expansion) if no double-click occurred.
type DelayedClickMsg struct {
	ClickID int
	ItemIdx int
	X, Y    int
}

// scrollbarHideMsg is sent to hide the scrollbar after the timeout period.
type scrollbarHideMsg struct {
	seq int // sequence number to ignore stale messages
}

// sidebarScrollbarHideMsg is sent to hide the sidebar scrollbar after timeout.
type sidebarScrollbarHideMsg struct {
	seq int
}

// scrollbarHideCmd returns a command that sends a scrollbarHideMsg after the timeout.
func scrollbarHideCmd(seq int) tea.Cmd {
	return tea.Tick(scrollbarHideDuration, func(_ time.Time) tea.Msg {
		return scrollbarHideMsg{seq: seq}
	})
}

// resizeSettleDuration is how long after the last resize event the chat
// waits before it starts warming the message cache it skipped mid-drag.
const resizeSettleDuration = 120 * time.Millisecond

// warmBatchSize is how many messages the chat renders into the width cache
// per warming step. Kept small so no single step blocks the UI thread for
// more than a frame or so, even on slow-to-render items.
const warmBatchSize = 25

// chatWarmMsg drives one incremental cache-warming step. The first one is
// delayed until the resize settles; the rest fire immediately, one per
// batch, so warming spreads across frames instead of blocking.
type chatWarmMsg struct {
	seq int // guards against stale timers from superseded resizes
}

// chatWarmCmd schedules the next warming step after delay (zero fires as
// soon as the runtime delivers it).
func chatWarmCmd(seq int, delay time.Duration) tea.Cmd {
	if delay <= 0 {
		return func() tea.Msg { return chatWarmMsg{seq: seq} }
	}
	return tea.Tick(delay, func(_ time.Time) tea.Msg {
		return chatWarmMsg{seq: seq}
	})
}

// sidebarScrollbarHideCmd returns a command that sends a sidebarScrollbarHideMsg
// after the timeout.
func sidebarScrollbarHideCmd(seq int) tea.Cmd {
	return tea.Tick(scrollbarHideDuration, func(_ time.Time) tea.Msg {
		return sidebarScrollbarHideMsg{seq: seq}
	})
}

// Chat represents the chat UI model that handles chat interactions and
// messages.
type Chat struct {
	com      *common.Common
	list     *list.List
	idInxMap map[string]int // Map of message IDs to their indices in the list

	// Animation visibility optimization: track animations paused due to items
	// being scrolled out of view. When items become visible again, their
	// animations are restarted.
	pausedAnimations map[string]struct{}

	// Mouse state
	mouseDown     bool
	mouseDownItem int // Item index where mouse was pressed
	mouseDownX    int // X position in item content (character offset)
	mouseDownY    int // Y position in item (line offset)
	mouseDragItem int // Current item index being dragged over
	mouseDragX    int // Current X in item content
	mouseDragY    int // Current Y in item

	// Scrollbar drag state. Independent of the text-selection mouseDown*
	// fields above — a click on the scrollbar column takes priority over
	// text selection and never sets mouseDown.
	scrollbarShown        bool // whether the scrollbar was drawn on the last Draw
	scrollbarDragging     bool
	scrollbarHover        bool
	scrollbarColX         int // relative x of the scrollbar's single column (from Draw's area origin)
	scrollbarTrackHeight  int // track height used for the last Draw's thumb geometry
	scrollbarThumbStart   int
	scrollbarThumbSize    int
	scrollbarContentSize  int
	scrollbarViewportSize int
	scrollbarDragAnchor   int // y offset within the thumb where the drag grabbed it

	// Click tracking for double/triple clicks
	lastClickTime time.Time
	lastClickX    int
	lastClickY    int
	clickCount    int

	// Pending single click action (delayed to detect double-click)
	pendingClickID int // Incremented on each click to invalidate old pending clicks

	// follow is a flag to indicate whether the view should auto-scroll to
	// bottom on new messages.
	follow bool

	// drawCache memoizes the decoded form of the last list.Render output so
	// repeat frames with byte-identical content skip the per-cell ANSI
	// reparse that uv.StyledString.Draw performs every call. See F9
	// (docs/notes/2026-05-12-chat-rendering-perf.md §4.8). Bounded to one
	// entry; invalidated implicitly by string inequality on the next Draw.
	drawCache *chatDrawCache

	// Scrollbar visibility state
	scrollbarVisible bool
	scrollbarHideSeq int    // current sequence number for hide timer
	scrollbarMode    string // "default", "always", or "never"

	// resizing suppresses the O(N) total-height scan while a resize is in
	// flight (and during the incremental warm afterward), so a drag only
	// reflows the visible items. resizeSettleSeq guards stale settle/warm
	// timers; warmNext tracks warming progress through the message list.
	resizing        bool
	resizeSettleSeq int
	warmNext        int
}

// scrollbarHideDuration is how long the scrollbar remains visible after scroll activity.
const scrollbarHideDuration = 2 * time.Second

// chatDrawCache holds the pre-decoded form of the last list.Render output.
// The cache is keyed by the rendered string and the screen's width method
// (graphemes vs wcwidth pick different decoders inside ultraviolet's
// printString, so a cached buffer is only valid for the method it was
// decoded with). The cached buffer is independent of the draw area, so
// resize / scroll changes that produce the same string still hit. We cannot
// use uv.StyledString.Lines because it bottoms out at the first iteration
// against a zero-bounds rectangle (see ultraviolet styled.go line 45 — the
// shared printString loop's `y >= bounds.Max.Y` exit applies to the
// line-building branch too). A Buffer of the rendered text's natural
// dimensions is the cheapest correct shape: StyledString.Draw runs once on
// miss to populate it, and Buffer.Draw is an O(cells) cell copy with no
// ANSI re-parse on hit.
type chatDrawCache struct {
	rendered string
	method   ansi.Method
	buf      uv.ScreenBuffer
}

// NewChat creates a new instance of [Chat] that handles chat interactions and
// messages.
func NewChat(com *common.Common, scrollbarMode string) *Chat {
	c := &Chat{
		com:              com,
		idInxMap:         make(map[string]int),
		pausedAnimations: make(map[string]struct{}),
		scrollbarMode:    scrollbarMode,
	}
	l := list.NewList()
	l.SetGap(1)
	l.RegisterRenderCallback(c.applyHighlightRange)
	l.RegisterRenderCallback(list.FocusedRenderCallback(l))
	c.list = l
	c.mouseDownItem = -1
	c.mouseDragItem = -1
	return c
}

// Height returns the height of the chat view port.
func (m *Chat) Height() int {
	return m.list.Height()
}

// Draw renders the chat UI component to the screen and the given area.
//
// The list's rendered output is cached in decoded form (see chatDrawCache) so
// that frames with byte-identical content skip the ANSI reparse that
// uv.StyledString.Draw performs on every call. The cache is keyed by the
// rendered string and the screen's width method; area / scroll changes do not
// invalidate it.
func (m *Chat) Draw(scr uv.Screen, area uv.Rectangle) {
	// Determine scrollbar visibility. Skip it entirely while resizing: the
	// thumb needs the exact total height (O(N) after a width change), which
	// is the dominant resize cost. It returns once the resize settles and
	// the cache has been warmed. The needs-scrollbar test itself uses the
	// cheap bounded overflow check.
	listHeight := m.list.Height() - 1
	needsScrollbar := false
	if !m.resizing {
		needsScrollbar = m.list.Overflows(m.list.Height())
	}

	// Determine visibility based on scrollbar mode.
	showScrollbar := false
	switch m.scrollbarMode {
	case config.ScrollbarAlways:
		showScrollbar = needsScrollbar
	case config.ScrollbarDefault:
		showScrollbar = needsScrollbar && m.scrollbarVisible
	case config.ScrollbarNever:
		showScrollbar = false
	}

	// Reserve space for scrollbar only when visible.
	scrollbarWidth := 0
	if showScrollbar {
		scrollbarWidth = 1
	}

	// Adjust list width to reserve space for scrollbar.
	listArea := area
	if scrollbarWidth > 0 {
		listArea.Max.X -= scrollbarWidth
	}

	rendered := m.list.Render()
	method, ok := scr.WidthMethod().(ansi.Method)
	if !ok {
		// Width method isn't an ansi.Method (unlikely in practice — both
		// TerminalScreen and ScreenBuffer store ansi.Method). Fall back
		// to the uncached path so behavior matches upstream exactly.
		uv.NewStyledString(rendered).Draw(scr, listArea)
	} else {
		if m.drawCache == nil ||
			m.drawCache.rendered != rendered ||
			m.drawCache.method != method {
			m.drawCache = newChatDrawCache(rendered, method)
		}
		drawCachedBuffer(scr, listArea, m.drawCache.buf)
	}

	// Draw scrollbar if visible and needed. Only reached when not resizing
	// (showScrollbar requires it), so TotalHeight is already computed and
	// cached above.
	if scrollbarWidth > 0 {
		contentSize := m.list.TotalHeight() - 1
		offset := m.list.Offset()
		hovered := m.scrollbarHover || m.scrollbarDragging
		scrollbar := common.ScrollbarStyled(m.com.Styles, listHeight, contentSize, listHeight, offset, hovered)
		if scrollbar != "" {
			scrollbarArea := image.Rectangle{
				Min: image.Point{X: area.Max.X - scrollbarWidth, Y: area.Min.Y},
				Max: image.Point{X: area.Max.X, Y: area.Max.Y},
			}
			uv.NewStyledString(scrollbar).Draw(scr, scrollbarArea)

			// Cache the thumb geometry (in coordinates relative to area's
			// origin, matching the mouse handlers' x/y) for hit-testing in
			// HandleScrollbarMouseDown/Drag.
			m.scrollbarShown = true
			m.scrollbarColX = area.Dx() - scrollbarWidth
			m.scrollbarTrackHeight = listHeight
			m.scrollbarContentSize = contentSize
			m.scrollbarViewportSize = listHeight
			m.scrollbarThumbStart, m.scrollbarThumbSize, _ = common.ScrollbarThumbBounds(listHeight, contentSize, listHeight, offset)
		} else {
			m.scrollbarShown = false
		}
	} else {
		// Not shown this frame. Leave scrollbarDragging alone — a drag that's
		// mid-flight should be allowed to finish even if a frame briefly
		// reports not-shown; only an explicit mouse-up ends it.
		m.scrollbarShown = false
	}
}

// newChatDrawCache builds a chatDrawCache for the given rendered string by
// running uv.StyledString.Draw into a fresh buffer sized to the text's
// natural bounds under the active width method. This is the only place
// ANSI decoding happens for cached frames — subsequent draws reuse buf
// via drawCachedBuffer.
//
// We can't use uv.StyledString.Bounds() here: it is hard-coded to
// ansi.GraphemeWidth, while StyledString.Draw lays cells using the
// destination buffer's WidthMethod (which we capture in `method`). For
// strings where graphemes and wcwidth disagree (emoji ZWJ sequences,
// some CJK, certain combining marks) the two answers diverge, leaving
// the cached buffer either too small (trailing cells dropped on hit) or
// too large (dead cells past the live content). Computing dimensions
// with `method.StringWidth` per line matches what printString tallies
// cell-by-cell, since both decode ANSI sequences and use the same width
// method.
func newChatDrawCache(rendered string, method ansi.Method) *chatDrawCache {
	w, h := renderedBounds(rendered, method)
	if w <= 0 {
		w = 1
	}
	if h <= 0 {
		h = 1
	}
	buf := uv.NewScreenBuffer(w, h)
	buf.Method = method
	uv.NewStyledString(rendered).Draw(buf, buf.Bounds())
	return &chatDrawCache{
		rendered: rendered,
		method:   method,
		buf:      buf,
	}
}

// renderedBounds returns the (width, height) cell extent of rendered
// when laid out by method. Width is the widest line's StringWidth (which
// strips ANSI sequences and tallies cells via method, exactly like
// printString); height is the line count. Both match what
// uv.StyledString.Draw will write into a buffer whose WidthMethod is
// method, so the cache buffer is always sized to fit the live content.
func renderedBounds(rendered string, method ansi.Method) (w, h int) {
	for line := range strings.SplitSeq(rendered, "\n") {
		w = max(w, method.StringWidth(line))
		h++
	}
	return w, h
}

// drawCachedBuffer blits a previously-decoded buffer into scr at area,
// mirroring uv.StyledString.Draw's screen-mode behavior for the default
// Wrap=false, Tail="" case. The clear loop matches StyledString.Draw line
// 51-56; the buf.Draw call replaces the per-cell ANSI decode that
// printString does on every uncached frame with a pure cell copy.
func drawCachedBuffer(scr uv.Screen, area uv.Rectangle, buf uv.ScreenBuffer) {
	// Clear the area first to match StyledString.Draw — leftover cells
	// from a previous frame outside the new content must be zeroed,
	// because Buffer.Draw skips empty cells (it doesn't clear).
	for y := area.Min.Y; y < area.Max.Y; y++ {
		for x := area.Min.X; x < area.Max.X; x++ {
			scr.SetCell(x, y, nil)
		}
	}
	buf.Draw(scr, area)
}

// BeginResize marks the chat as actively resizing so the next draws skip
// the full-height scan (and the scrollbar), reflowing only the visible
// items. It returns a command that, once resizing settles, starts warming
// the cache so the scrollbar can recompute without blocking.
func (m *Chat) BeginResize() tea.Cmd {
	m.resizing = true
	m.resizeSettleSeq++
	m.warmNext = 0
	return chatWarmCmd(m.resizeSettleSeq, resizeSettleDuration)
}

// WarmStep renders the next batch of messages into the width cache and
// returns a command to continue warming plus whether warming finished. On
// completion the resize suppression is cleared so the next draw recomputes
// the (now instant) total height and scrollbar. A stale seq — from a resize
// that has since been superseded — is a no-op returning (nil, false).
func (m *Chat) WarmStep(seq int) (cmd tea.Cmd, done bool) {
	if seq != m.resizeSettleSeq {
		return nil, false
	}
	m.warmNext = m.list.Prewarm(m.warmNext, warmBatchSize)
	if m.warmNext >= m.list.Len() {
		m.resizing = false
		return nil, true
	}
	return chatWarmCmd(seq, 0), false
}

// SetSize sets the size of the chat view port.
func (m *Chat) SetSize(width, height int) {
	// Reserve a column for the scrollbar when content overflows, decided
	// with a cheap bounded overflow check rather than the O(N) total height.
	// The final width is applied in a single SetSize so that an unchanged
	// width is a no-op — critical after warming, where re-setting the same
	// width would otherwise drop the freshly warmed cache and reintroduce
	// the blocking full render.
	// Capture whether we should stay pinned to the bottom *before* the size
	// change. A width change rewraps every item, so the list's line offsets
	// (offsetIdx/offsetLine) become stale and AtBottom() can no longer be
	// trusted afterward. follow short-circuits the AtBottom() walk in the
	// common streaming case.
	wasFollowing := m.follow || m.AtBottom()
	listWidth := width
	if m.list.Overflows(height) {
		listWidth = max(0, width-1)
	}
	m.list.SetSize(listWidth, height)
	// Re-anchor to bottom if we were pinned there before the resize.
	if wasFollowing {
		m.ScrollToBottom()
	}
}

// Len returns the number of items in the chat list.
func (m *Chat) Len() int {
	return m.list.Len()
}

// InvalidateRenderCaches drops cached rendered output on every message
// item so the next draw re-renders with the current styles.
func (m *Chat) InvalidateRenderCaches() {
	items := make([]chat.MessageItem, 0, m.list.Len())
	for i := range m.list.Len() {
		if item, ok := m.list.ItemAt(i).(chat.MessageItem); ok {
			items = append(items, item)
		}
	}
	chat.ClearItemCaches(items)
}

// SetMessages sets the chat messages to the provided list of message items.
func (m *Chat) SetMessages(msgs ...chat.MessageItem) tea.Cmd {
	m.idInxMap = make(map[string]int)
	m.pausedAnimations = make(map[string]struct{})
	m.scrollbarVisible = false // Reset scrollbar visibility on new session load

	items := make([]list.Item, len(msgs))
	for i, msg := range msgs {
		m.idInxMap[msg.ID()] = i
		// Register nested tool IDs for tools that contain nested tools.
		if container, ok := msg.(chat.NestedToolContainer); ok {
			for _, nested := range container.NestedTools() {
				m.idInxMap[nested.ID()] = i
			}
		}
		items[i] = msg
	}
	m.list.SetItems(items...)
	m.ScrollToBottom()
	return nil
}

// AppendMessages appends a new message item to the chat list.
func (m *Chat) AppendMessages(msgs ...chat.MessageItem) {
	items := make([]list.Item, len(msgs))
	indexOffset := m.list.Len()
	for i, msg := range msgs {
		m.idInxMap[msg.ID()] = indexOffset + i
		// Register nested tool IDs for tools that contain nested tools.
		if container, ok := msg.(chat.NestedToolContainer); ok {
			for _, nested := range container.NestedTools() {
				m.idInxMap[nested.ID()] = indexOffset + i
			}
		}
		items[i] = msg
	}
	m.list.AppendItems(items...)
}

// UpdateNestedToolIDs updates the ID map for nested tools within a container.
// Call this after modifying nested tools to ensure animations work correctly.
func (m *Chat) UpdateNestedToolIDs(containerID string) {
	idx, ok := m.idInxMap[containerID]
	if !ok {
		return
	}

	item, ok := m.list.ItemAt(idx).(chat.MessageItem)
	if !ok {
		return
	}

	container, ok := item.(chat.NestedToolContainer)
	if !ok {
		return
	}

	// Register all nested tool IDs to point to the container's index.
	for _, nested := range container.NestedTools() {
		m.idInxMap[nested.ID()] = idx
	}
}

// Animate animates items in the chat list. Only propagates animation messages
// to visible items to save CPU. When items are not visible, their animation ID
// is tracked so it can be restarted when they become visible again.
func (m *Chat) Animate(msg anim.StepMsg) tea.Cmd {
	idx, ok := m.idInxMap[msg.ID]
	if !ok {
		return nil
	}

	animatable, ok := m.list.ItemAt(idx).(chat.Animatable)
	if !ok {
		return nil
	}

	// Check if item is currently visible.
	startIdx, endIdx := m.list.VisibleItemIndices()
	isVisible := idx >= startIdx && idx <= endIdx

	if !isVisible {
		// Item not visible - pause animation by not propagating.
		// Track it so we can restart when it becomes visible.
		m.pausedAnimations[msg.ID] = struct{}{}
		return nil
	}

	// Item is visible - remove from paused set and animate.
	delete(m.pausedAnimations, msg.ID)
	return animatable.Animate(msg)
}

// RestartPausedVisibleAnimations restarts animations for items that were paused
// due to being scrolled out of view but are now visible again.
func (m *Chat) RestartPausedVisibleAnimations() tea.Cmd {
	if len(m.pausedAnimations) == 0 {
		return nil
	}

	startIdx, endIdx := m.list.VisibleItemIndices()
	var cmds []tea.Cmd

	for id := range m.pausedAnimations {
		idx, ok := m.idInxMap[id]
		if !ok {
			// Item no longer exists.
			delete(m.pausedAnimations, id)
			continue
		}

		if idx >= startIdx && idx <= endIdx {
			// Item is now visible - restart its animation.
			if animatable, ok := m.list.ItemAt(idx).(chat.Animatable); ok {
				if cmd := animatable.StartAnimation(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
			delete(m.pausedAnimations, id)
		}
	}

	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// Focus sets the focus state of the chat component.
func (m *Chat) Focus() {
	m.list.Focus()
}

// Blur removes the focus state from the chat component.
func (m *Chat) Blur() {
	m.list.Blur()
}

// AtBottom returns whether the chat list is currently scrolled to the bottom.
func (m *Chat) AtBottom() bool {
	return m.list.AtBottom()
}

// Follow returns whether the chat view is in follow mode (auto-scroll to
// bottom on new messages).
func (m *Chat) Follow() bool {
	return m.follow
}

// ScrollToBottom scrolls the chat view to the bottom.
// Does not trigger scrollbar visibility (auto-scroll to show new content).
func (m *Chat) ScrollToBottom() tea.Cmd {
	m.list.ScrollToBottom()
	m.follow = true
	return nil
}

// ScrollToTop scrolls the chat view to the top.
func (m *Chat) ScrollToTop() tea.Cmd {
	m.list.ScrollToTop()
	m.follow = false // Disable follow mode when user scrolls up
	return m.showScrollbar()
}

// ScrollBy scrolls the chat view by the given number of line deltas.
func (m *Chat) ScrollBy(lines int) tea.Cmd {
	m.list.ScrollBy(lines)
	if lines < 0 {
		// Scrolling up always disables follow mode.
		m.follow = false
	} else if m.AtBottom() {
		// Scrolling down re-enables follow when we reach the bottom.
		m.follow = true
	}
	return m.showScrollbar()
}

// ScrollToSelected scrolls the chat view to the selected item.
func (m *Chat) ScrollToSelected() tea.Cmd {
	m.list.ScrollToSelected()
	m.follow = m.AtBottom() // Disable follow mode if user scrolls up
	return m.showScrollbar()
}

// ScrollToIndex scrolls the chat view to the item at the given index.
func (m *Chat) ScrollToIndex(index int) tea.Cmd {
	m.list.ScrollToIndex(index)
	m.follow = m.AtBottom() // Disable follow mode if user scrolls up
	return m.showScrollbar()
}

// showScrollbar makes the scrollbar visible and returns a command to hide it after timeout.
func (m *Chat) showScrollbar() tea.Cmd {
	// Only start timer for "default" mode
	if m.scrollbarMode != config.ScrollbarDefault {
		return nil
	}
	m.scrollbarVisible = true
	m.scrollbarHideSeq++
	return scrollbarHideCmd(m.scrollbarHideSeq)
}

// HideScrollbar hides the scrollbar if the sequence matches.
func (m *Chat) HideScrollbar(seq int) {
	// Only hide scrollbar for "default" mode
	if m.scrollbarMode != config.ScrollbarDefault {
		return
	}
	if seq == m.scrollbarHideSeq {
		m.scrollbarVisible = false
	}
}

// ScrollToTopAndAnimate scrolls the chat view to the top and returns a command to restart
// any paused animations that are now visible.
func (m *Chat) ScrollToTopAndAnimate() tea.Cmd {
	return tea.Batch(m.ScrollToTop(), m.RestartPausedVisibleAnimations())
}

// ScrollToBottomAndAnimate scrolls the chat view to the bottom and returns a command to
// restart any paused animations that are now visible.
func (m *Chat) ScrollToBottomAndAnimate() tea.Cmd {
	return tea.Batch(m.ScrollToBottom(), m.RestartPausedVisibleAnimations())
}

// ScrollByAndAnimate scrolls the chat view by the given number of line deltas and returns
// a command to restart any paused animations that are now visible.
func (m *Chat) ScrollByAndAnimate(lines int) tea.Cmd {
	return tea.Batch(m.ScrollBy(lines), m.RestartPausedVisibleAnimations())
}

// ScrollToSelectedAndAnimate scrolls the chat view to the selected item and returns a
// command to restart any paused animations that are now visible.
func (m *Chat) ScrollToSelectedAndAnimate() tea.Cmd {
	return tea.Batch(m.ScrollToSelected(), m.RestartPausedVisibleAnimations())
}

// SelectedItemInView returns whether the selected item is currently in view.
func (m *Chat) SelectedItemInView() bool {
	return m.list.SelectedItemInView()
}

func (m *Chat) isSelectable(index int) bool {
	item := m.list.ItemAt(index)
	if item == nil {
		return false
	}
	_, ok := item.(list.Focusable)
	return ok
}

// SetSelected sets the selected message index in the chat list.
func (m *Chat) SetSelected(index int) {
	m.list.SetSelected(index)
	if index < 0 || index >= m.list.Len() {
		return
	}
	for {
		if m.isSelectable(m.list.Selected()) {
			return
		}
		if m.list.SelectNext() {
			continue
		}
		// If we're at the end and the last item isn't selectable, walk backwards
		// to find the nearest selectable item.
		for {
			if !m.list.SelectPrev() {
				return
			}
			if m.isSelectable(m.list.Selected()) {
				return
			}
		}
	}
}

// SelectPrev selects the previous message in the chat list.
func (m *Chat) SelectPrev() {
	for {
		if !m.list.SelectPrev() {
			return
		}
		if m.isSelectable(m.list.Selected()) {
			return
		}
	}
}

// SelectNext selects the next message in the chat list.
func (m *Chat) SelectNext() {
	for {
		if !m.list.SelectNext() {
			return
		}
		if m.isSelectable(m.list.Selected()) {
			return
		}
	}
}

// SelectFirst selects the first message in the chat list.
func (m *Chat) SelectFirst() {
	if !m.list.SelectFirst() {
		return
	}
	if m.isSelectable(m.list.Selected()) {
		return
	}
	for {
		if !m.list.SelectNext() {
			return
		}
		if m.isSelectable(m.list.Selected()) {
			return
		}
	}
}

// SelectLast selects the last message in the chat list.
func (m *Chat) SelectLast() {
	if !m.list.SelectLast() {
		return
	}
	if m.isSelectable(m.list.Selected()) {
		return
	}
	for {
		if !m.list.SelectPrev() {
			return
		}
		if m.isSelectable(m.list.Selected()) {
			return
		}
	}
}

// SelectFirstInView selects the first message currently in view.
func (m *Chat) SelectFirstInView() {
	startIdx, endIdx := m.list.VisibleItemIndices()
	for i := startIdx; i <= endIdx; i++ {
		if m.isSelectable(i) {
			m.list.SetSelected(i)
			return
		}
	}
}

// SelectLastInView selects the last message currently in view.
func (m *Chat) SelectLastInView() {
	startIdx, endIdx := m.list.VisibleItemIndices()
	for i := endIdx; i >= startIdx; i-- {
		if m.isSelectable(i) {
			m.list.SetSelected(i)
			return
		}
	}
}

// ClearMessages removes all messages from the chat list.
func (m *Chat) ClearMessages() {
	m.idInxMap = make(map[string]int)
	m.pausedAnimations = make(map[string]struct{})
	m.scrollbarVisible = false
	m.list.SetItems()
	m.ClearMouse()
}

// RemoveMessage removes a message from the chat list by its ID.
func (m *Chat) RemoveMessage(id string) {
	idx, ok := m.idInxMap[id]
	if !ok {
		return
	}

	// Remove from list
	m.list.RemoveItem(idx)

	// Remove from index map
	delete(m.idInxMap, id)

	// Rebuild index map for all items after the removed one
	for i := idx; i < m.list.Len(); i++ {
		if item, ok := m.list.ItemAt(i).(chat.MessageItem); ok {
			m.idInxMap[item.ID()] = i
		}
	}

	// Clean up any paused animations for this message
	delete(m.pausedAnimations, id)
}

// MessageItem returns the message item with the given ID, or nil if not found.
func (m *Chat) MessageItem(id string) chat.MessageItem {
	idx, ok := m.idInxMap[id]
	if !ok {
		return nil
	}
	item, ok := m.list.ItemAt(idx).(chat.MessageItem)
	if !ok {
		return nil
	}
	return item
}

// ToggleExpandedSelectedItem expands the selected message item if it is expandable.
func (m *Chat) ToggleExpandedSelectedItem() {
	if expandable, ok := m.list.SelectedItem().(chat.Expandable); ok {
		wasFollowing := m.follow
		if !expandable.ToggleExpanded() {
			m.ScrollToIndex(m.list.Selected())
		}
		if wasFollowing {
			m.ScrollToBottom()
		}
	}
}

// SelectedNestedToolContainer returns the message ID and tool-call ID of
// the currently selected chat item, if it's a nested-tool container
// (agent / agentic_fetch delegation) — used by alt+down to start
// child-session navigation. ok is false for any other selection.
func (m *Chat) SelectedNestedToolContainer() (messageID, toolCallID string, ok bool) {
	item := m.list.SelectedItem()
	toolItem, isTool := item.(chat.ToolMessageItem)
	if !isTool {
		return "", "", false
	}
	if _, isContainer := item.(chat.NestedToolContainer); !isContainer {
		return "", "", false
	}
	return toolItem.MessageID(), toolItem.ToolCall().ID, true
}

// PlainToolItemAt reports whether the chat item at viewport-relative
// position (x, y) is a tool call with no click behavior left — a
// ToolMessageItem that is not a NestedToolContainer (agent/agentic_fetch,
// which still drills into the child session on click). Used by
// handleClickFocus to keep a click on such a row from stealing focus away
// from the editor: since inline expand/preview was removed, there is
// nothing left to interact with there. Returns false for anything else
// (assistant text, todos, delegations, no item at that position, ...),
// which still focuses the chat pane as before.
func (m *Chat) PlainToolItemAt(x, y int) bool {
	itemIdx, _ := m.list.ItemIndexAtPosition(x, y)
	if itemIdx < 0 {
		return false
	}
	item := m.list.ItemAt(itemIdx)
	if _, isTool := item.(chat.ToolMessageItem); !isTool {
		return false
	}
	_, isContainer := item.(chat.NestedToolContainer)
	return !isContainer
}

// NestedToolContainerRefs returns, in list order, the message ID and
// tool-call ID of every top-level chat item that is a nested-tool
// container (agent / agentic_fetch delegation). Used to build the
// sibling list for alt+left/alt+right session-navigation cycling.
func (m *Chat) NestedToolContainerRefs() []childSessionRef {
	refs := make([]childSessionRef, 0)
	for i := range m.list.Len() {
		item := m.list.ItemAt(i)
		toolItem, isTool := item.(chat.ToolMessageItem)
		if !isTool {
			continue
		}
		if _, isContainer := item.(chat.NestedToolContainer); !isContainer {
			continue
		}
		refs = append(refs, childSessionRef{messageID: toolItem.MessageID(), toolCallID: toolItem.ToolCall().ID})
	}
	return refs
}

// IsSelectedShellItem returns true if the currently selected item is a
// ShellItem (bang-mode result).
func (m *Chat) IsSelectedShellItem() bool {
	_, ok := m.list.SelectedItem().(*chat.ShellItem)
	return ok
}

// ScrollSelectedShellHorizontal scrolls the selected ShellItem horizontally
// by delta columns. No-op if the selected item is not a ShellItem.
func (m *Chat) ScrollSelectedShellHorizontal(delta int) {
	if shell, ok := m.list.SelectedItem().(*chat.ShellItem); ok {
		shell.ScrollHorizontal(delta)
	}
}

// HandleKeyMsg handles key events for the chat component.
func (m *Chat) HandleKeyMsg(key tea.KeyMsg) (bool, tea.Cmd) {
	if m.list.Focused() {
		if handler, ok := m.list.SelectedItem().(chat.KeyEventHandler); ok {
			return handler.HandleKeyEvent(key)
		}
	}
	return false, nil
}

// HandleMouseDown handles mouse down events for the chat component.
// It detects single, double, and triple clicks for text selection.
// Returns whether the click was handled and an optional command for delayed
// single-click actions.
func (m *Chat) HandleMouseDown(x, y int) (bool, tea.Cmd) {
	if m.list.Len() == 0 {
		return false, nil
	}

	itemIdx, itemY := m.list.ItemIndexAtPosition(x, y)
	if itemIdx < 0 {
		return false, nil
	}
	if !m.isSelectable(itemIdx) {
		return false, nil
	}

	// Increment pending click ID to invalidate any previous pending clicks.
	m.pendingClickID++
	clickID := m.pendingClickID

	// Detect multi-click (double/triple)
	now := time.Now()
	if now.Sub(m.lastClickTime) <= doubleClickThreshold &&
		abs(x-m.lastClickX) <= clickTolerance &&
		abs(y-m.lastClickY) <= clickTolerance {
		m.clickCount++
	} else {
		m.clickCount = 1
	}
	m.lastClickTime = now
	m.lastClickX = x
	m.lastClickY = y

	// Note: this deliberately does not call m.list.SetSelected(itemIdx).
	// selectedIdx drives the keyboard "browse mode" focus indicator (the
	// bordered highlight painted by list.FocusedRenderCallback) as well as
	// arrow-key/alt+down navigation; it used to also move here so that
	// click-to-expand could act on "the selected item". Click-to-expand is
	// gone (see PlainToolItemAt), and mouse clicks must never move keyboard
	// focus (see uiFocusState's doc comment) — so moving selectedIdx on
	// every click just painted a stray focus stripe under the cursor with
	// no keyboard-navigation meaning behind it. Click-driven behavior below
	// uses itemIdx directly instead.
	var cmd tea.Cmd

	switch m.clickCount {
	case 1:
		// Single click - start selection and schedule delayed click action.
		m.mouseDown = true
		m.mouseDownItem = itemIdx
		m.mouseDownX = x
		m.mouseDownY = itemY
		m.mouseDragItem = itemIdx
		m.mouseDragX = x
		m.mouseDragY = itemY

		// Schedule delayed click action (e.g., expansion) after a short delay.
		// If a double-click occurs, the clickID will be invalidated.
		cmd = tea.Tick(doubleClickThreshold, func(t time.Time) tea.Msg {
			return DelayedClickMsg{
				ClickID: clickID,
				ItemIdx: itemIdx,
				X:       x,
				Y:       itemY,
			}
		})
	case 2:
		// Double click - select word (no delayed action)
		m.selectWord(itemIdx, x, itemY)
	case 3:
		// Triple click - select line (no delayed action)
		m.selectLine(itemIdx, itemY)
		m.clickCount = 0 // Reset after triple click
	}

	return true, cmd
}

// HandleDelayedClick handles a delayed single-click action (like expansion
// or entering a child session). It only executes if the click ID matches
// (i.e., no double-click occurred) and no text selection was made (drag to
// select). openContainer is true when the click landed on a nested-tool
// container (agent / agentic_fetch delegation) — callers should navigate
// into the child session instead of toggling expansion, using the returned
// messageID/toolCallID. These are resolved from the clicked item directly
// (not m.chat.SelectedNestedToolContainer, which reads the keyboard-driven
// selection) because a mouse click no longer moves that selection — see
// HandleMouseDown.
func (m *Chat) HandleDelayedClick(msg DelayedClickMsg) (handled, openContainer bool, messageID, toolCallID string) {
	// Ignore if this click was superseded by a newer click (double/triple).
	if msg.ClickID != m.pendingClickID {
		return false, false, "", ""
	}

	// Don't expand if user dragged to select text.
	if m.HasHighlight() {
		return false, false, "", ""
	}

	// Execute the click action (e.g., expansion). Look the item up by the
	// clicked index directly rather than via m.list.SelectedItem() — mouse
	// clicks don't move the keyboard-selected item (see HandleMouseDown).
	clickedItem := m.list.ItemAt(msg.ItemIdx)
	if clickable, ok := clickedItem.(list.MouseClickable); ok {
		handled := clickable.HandleMouseClick(ansi.MouseButton1, msg.X, msg.Y)
		if !handled {
			return false, false, "", ""
		}
		// A click on a nested-tool container navigates into the child
		// session rather than expanding — skip the Expandable branch
		// entirely.
		if _, isContainer := clickedItem.(chat.NestedToolContainer); isContainer {
			if toolItem, ok := clickedItem.(chat.ToolMessageItem); ok {
				return true, true, toolItem.MessageID(), toolItem.ToolCall().ID
			}
			return true, false, "", ""
		}
		// Toggle expansion only when the item signalled it handled the
		// click. Items like AssistantMessageItem only report handled when
		// the click is on their expandable region, so this avoids
		// toggling expansion for clicks outside the clickable area.
		if expandable, ok := clickedItem.(chat.Expandable); ok {
			wasFollowing := m.follow
			if !expandable.ToggleExpanded() {
				m.ScrollToIndex(msg.ItemIdx)
			}
			if wasFollowing {
				m.ScrollToBottom()
			}
		}
		return true, false, "", ""
	}

	return false, false, "", ""
}

// HandleMouseUp handles mouse up events for the chat component.
func (m *Chat) HandleMouseUp(x, y int) bool {
	if !m.mouseDown {
		return false
	}

	m.mouseDown = false
	return true
}

// HandleMouseDrag handles mouse drag events for the chat component.
func (m *Chat) HandleMouseDrag(x, y int) bool {
	if !m.mouseDown {
		return false
	}

	if m.list.Len() == 0 {
		return false
	}

	itemIdx, itemY := m.list.ItemIndexAtPosition(x, y)
	if itemIdx < 0 {
		return false
	}

	m.mouseDragItem = itemIdx
	m.mouseDragX = x
	m.mouseDragY = itemY

	return true
}

// HandleScrollbarMouseDown starts a scrollbar drag if (x, y) lands on the
// scrollbar's column, as computed by the last Draw. It takes priority over
// text selection — callers should try this before HandleMouseDown and only
// fall through to it when this returns false.
func (m *Chat) HandleScrollbarMouseDown(x, y int) (bool, tea.Cmd) {
	if !m.scrollbarShown || x != m.scrollbarColX || y < 0 || y >= m.scrollbarTrackHeight {
		return false, nil
	}

	if y >= m.scrollbarThumbStart && y < m.scrollbarThumbStart+m.scrollbarThumbSize {
		// Grabbed inside the thumb: keep the same offset within it so the
		// thumb doesn't jump under the cursor.
		m.scrollbarDragAnchor = y - m.scrollbarThumbStart
	} else {
		// Clicked on empty track: center the thumb under the cursor.
		m.scrollbarDragAnchor = m.scrollbarThumbSize / 2
	}
	m.scrollbarDragging = true

	return true, m.dragScrollbarTo(y)
}

// HandleScrollbarMouseDrag continues an in-progress scrollbar drag, scrolling
// the chat so the thumb tracks the cursor's Y position. x is ignored — the
// scrollbar only drags vertically.
func (m *Chat) HandleScrollbarMouseDrag(x, y int) (bool, tea.Cmd) {
	if !m.scrollbarDragging {
		return false, nil
	}
	return true, m.dragScrollbarTo(y)
}

// HandleScrollbarMouseUp ends an in-progress scrollbar drag.
func (m *Chat) HandleScrollbarMouseUp() bool {
	if !m.scrollbarDragging {
		return false
	}
	m.scrollbarDragging = false
	return true
}

// ScrollbarHoverAt updates and returns whether (x, y) is hovering the
// scrollbar's column, for hover-highlight rendering from mouse-motion events
// that aren't part of a drag.
func (m *Chat) ScrollbarHoverAt(x, y int) bool {
	m.scrollbarHover = m.scrollbarShown && x == m.scrollbarColX && y >= 0 && y < m.scrollbarTrackHeight
	return m.scrollbarHover
}

// dragScrollbarTo scrolls the chat so the thumb's top sits at
// y-scrollbarDragAnchor, converting that target thumb position back to a
// content offset via common.ScrollbarOffsetForThumbStart and reaching it
// with ScrollBy (which also maintains the follow flag and scrollbar-
// visibility timer, same as wheel scrolling).
func (m *Chat) dragScrollbarTo(y int) tea.Cmd {
	thumbStart := y - m.scrollbarDragAnchor
	target := common.ScrollbarOffsetForThumbStart(thumbStart, m.scrollbarThumbSize, m.scrollbarTrackHeight, m.scrollbarContentSize, m.scrollbarViewportSize)
	return m.ScrollBy(target - m.list.Offset())
}

// HasHighlight returns whether there is currently highlighted content.
func (m *Chat) HasHighlight() bool {
	startItemIdx, startLine, startCol, endItemIdx, endLine, endCol := m.getHighlightRange()
	return startItemIdx >= 0 && endItemIdx >= 0 && (startLine != endLine || startCol != endCol)
}

// HighlightContent returns the currently highlighted content based on the mouse
// selection. It returns an empty string if no content is highlighted.
func (m *Chat) HighlightContent() string {
	startItemIdx, startLine, startCol, endItemIdx, endLine, endCol := m.getHighlightRange()
	if startItemIdx < 0 || endItemIdx < 0 || startLine == endLine && startCol == endCol {
		return ""
	}

	var sb strings.Builder
	for i := startItemIdx; i <= endItemIdx; i++ {
		item := m.list.ItemAt(i)
		if hi, ok := item.(list.Highlightable); ok {
			startLine, startCol, endLine, endCol := hi.Highlight()
			listWidth := m.list.Width()
			var rendered string
			if rr, ok := item.(list.RawRenderable); ok {
				rendered = rr.RawRender(listWidth)
			} else {
				rendered = item.Render(listWidth)
			}
			sb.WriteString(list.HighlightContent(
				rendered,
				uv.Rect(0, 0, listWidth, lipgloss.Height(rendered)),
				startLine,
				startCol,
				endLine,
				endCol,
			))
			sb.WriteString(strings.Repeat("\n", m.list.Gap()))
		}
	}

	return strings.TrimSpace(sb.String())
}

// ClearMouse clears the current mouse interaction state.
func (m *Chat) ClearMouse() {
	m.mouseDown = false
	m.mouseDownItem = -1
	m.mouseDragItem = -1
	m.lastClickTime = time.Time{}
	m.lastClickX = 0
	m.lastClickY = 0
	m.clickCount = 0
	m.pendingClickID++ // Invalidate any pending delayed click
	m.scrollbarDragging = false
}

// applyHighlightRange applies the current highlight range to the chat items.
func (m *Chat) applyHighlightRange(idx, selectedIdx int, item list.Item) list.Item {
	if hi, ok := item.(list.Highlightable); ok {
		// Apply highlight
		startItemIdx, startLine, startCol, endItemIdx, endLine, endCol := m.getHighlightRange()
		sLine, sCol, eLine, eCol := -1, -1, -1, -1
		if idx >= startItemIdx && idx <= endItemIdx {
			if idx == startItemIdx && idx == endItemIdx {
				// Single item selection
				sLine = startLine
				sCol = startCol
				eLine = endLine
				eCol = endCol
			} else if idx == startItemIdx {
				// First item - from start position to end of item
				sLine = startLine
				sCol = startCol
				eLine = -1
				eCol = -1
			} else if idx == endItemIdx {
				// Last item - from start of item to end position
				sLine = 0
				sCol = 0
				eLine = endLine
				eCol = endCol
			} else {
				// Middle item - fully highlighted
				sLine = 0
				sCol = 0
				eLine = -1
				eCol = -1
			}
		}

		hi.SetHighlight(sLine, sCol, eLine, eCol)
		return hi.(list.Item)
	}

	return item
}

// getHighlightRange returns the current highlight range.
func (m *Chat) getHighlightRange() (startItemIdx, startLine, startCol, endItemIdx, endLine, endCol int) {
	if m.mouseDownItem < 0 {
		return -1, -1, -1, -1, -1, -1
	}

	downItemIdx := m.mouseDownItem
	dragItemIdx := m.mouseDragItem

	// Determine selection direction
	draggingDown := dragItemIdx > downItemIdx ||
		(dragItemIdx == downItemIdx && m.mouseDragY > m.mouseDownY) ||
		(dragItemIdx == downItemIdx && m.mouseDragY == m.mouseDownY && m.mouseDragX >= m.mouseDownX)

	if draggingDown {
		// Normal forward selection
		startItemIdx = downItemIdx
		startLine = m.mouseDownY
		startCol = m.mouseDownX
		endItemIdx = dragItemIdx
		endLine = m.mouseDragY
		endCol = m.mouseDragX
	} else {
		// Backward selection (dragging up)
		startItemIdx = dragItemIdx
		startLine = m.mouseDragY
		startCol = m.mouseDragX
		endItemIdx = downItemIdx
		endLine = m.mouseDownY
		endCol = m.mouseDownX
	}

	return startItemIdx, startLine, startCol, endItemIdx, endLine, endCol
}

// selectWord selects the word at the given position within an item.
func (m *Chat) selectWord(itemIdx, x, itemY int) {
	item := m.list.ItemAt(itemIdx)
	if item == nil {
		return
	}

	// Get the rendered content for this item
	var rendered string
	if rr, ok := item.(list.RawRenderable); ok {
		rendered = rr.RawRender(m.list.Width())
	} else {
		rendered = item.Render(m.list.Width())
	}

	lines := strings.Split(rendered, "\n")
	if itemY < 0 || itemY >= len(lines) {
		return
	}

	// Adjust x for the item's left padding (border + padding) to get content column.
	// The mouse x is in viewport space, but we need content space for boundary detection.
	offset := chat.MessageLeftPaddingTotal
	contentX := max(x-offset, 0)

	line := ansi.Strip(lines[itemY])
	startCol, endCol := findWordBoundaries(line, contentX)
	if startCol == endCol {
		// No word found at position, fallback to single click behavior
		m.mouseDown = true
		m.mouseDownItem = itemIdx
		m.mouseDownX = x
		m.mouseDownY = itemY
		m.mouseDragItem = itemIdx
		m.mouseDragX = x
		m.mouseDragY = itemY
		return
	}

	// Set selection to the word boundaries (convert back to viewport space).
	// Keep mouseDown true so HandleMouseUp triggers the copy.
	m.mouseDown = true
	m.mouseDownItem = itemIdx
	m.mouseDownX = startCol + offset
	m.mouseDownY = itemY
	m.mouseDragItem = itemIdx
	m.mouseDragX = endCol + offset
	m.mouseDragY = itemY
}

// selectLine selects the entire line at the given position within an item.
func (m *Chat) selectLine(itemIdx, itemY int) {
	item := m.list.ItemAt(itemIdx)
	if item == nil {
		return
	}

	// Get the rendered content for this item
	var rendered string
	if rr, ok := item.(list.RawRenderable); ok {
		rendered = rr.RawRender(m.list.Width())
	} else {
		rendered = item.Render(m.list.Width())
	}

	lines := strings.Split(rendered, "\n")
	if itemY < 0 || itemY >= len(lines) {
		return
	}

	// Get line length (stripped of ANSI codes) and account for padding.
	// SetHighlight will subtract the offset, so we need to add it here.
	offset := chat.MessageLeftPaddingTotal
	lineLen := ansi.StringWidth(lines[itemY])

	// Set selection to the entire line.
	// Keep mouseDown true so HandleMouseUp triggers the copy.
	m.mouseDown = true
	m.mouseDownItem = itemIdx
	m.mouseDownX = 0
	m.mouseDownY = itemY
	m.mouseDragItem = itemIdx
	m.mouseDragX = lineLen + offset
	m.mouseDragY = itemY
}

// findWordBoundaries finds the start and end column of the word at the given column.
// Returns (startCol, endCol) where endCol is exclusive.
func findWordBoundaries(line string, col int) (startCol, endCol int) {
	if line == "" || col < 0 {
		return 0, 0
	}

	i := displaywidth.StringGraphemes(line)
	for i.Next() {
	}

	// Segment the line into words using UAX#29.
	lineCol := 0 // tracks the visited column widths
	lastCol := 0 // tracks the start of the current token
	iter := words.FromString(line)
	for iter.Next() {
		token := iter.Value()
		tokenWidth := displaywidth.String(token)

		graphemeStart := lineCol
		graphemeEnd := lineCol + tokenWidth
		lineCol += tokenWidth

		// If clicked before this token, return the previous token boundaries.
		if col < graphemeStart {
			return lastCol, lastCol
		}

		// Update lastCol to the end of this token for next iteration.
		lastCol = graphemeEnd

		// If clicked within this token, return its boundaries.
		if col >= graphemeStart && col < graphemeEnd {
			// If clicked on whitespace, return empty selection.
			if strings.TrimSpace(token) == "" {
				return col, col
			}
			return graphemeStart, graphemeEnd
		}
	}

	return col, col
}

// abs returns the absolute value of an integer.
func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
