package model

import (
	"strconv"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// focusableChatItem is a one-line chat item the selection can land on, which
// testMessageItem is not (Chat.isSelectable only accepts list.Focusable).
type focusableChatItem struct {
	id   string
	text string
}

func (f *focusableChatItem) ID() string           { return f.id }
func (f *focusableChatItem) Render(int) string    { return f.text }
func (f *focusableChatItem) RawRender(int) string { return f.text }
func (f *focusableChatItem) Version() uint64      { return 0 }
func (f *focusableChatItem) Finished() bool       { return true }
func (f *focusableChatItem) SetFocused(bool)      {}

var _ chat.FocusableMessageItem = (*focusableChatItem)(nil)

// wheelTestUI is a chat sitting at the bottom of a transcript far taller than
// its viewport, with the last message selected — the state the wheel is
// almost always used from.
func wheelTestUI(t *testing.T) *UI {
	t.Helper()

	u := newTestUI()
	u.dialog = dialog.NewOverlay()
	u.sess.current = &session.Session{ID: "s1"}
	items := make([]chat.MessageItem, 0, 200)
	for i := range 200 {
		items = append(items, &focusableChatItem{id: strconv.Itoa(i), text: "message " + strconv.Itoa(i)})
	}
	u.chat.SetMessages(items...)
	u.updateLayoutAndSize()
	u.chat.ScrollToBottom()
	u.chat.SelectLast()
	require.True(t, u.chat.AtBottom(), "the fixture must start pinned to the bottom")
	return u
}

// wheel drives one coalesced wheel event over the chat area, the way the
// input filter delivers a physical tick.
func wheel(u *UI, deltaY float64) {
	u.Update(common.CoalescedWheelMsg{
		Mouse: tea.Mouse{
			X: u.lay.layout.main.Min.X + 1,
			Y: u.lay.layout.main.Min.Y + 1,
		},
		DeltaY: deltaY,
	})
}

// TestWheelScrollUpKeepsMovingTheViewport is the regression test for a chat
// the wheel could not scroll: each tick scrolled up, noticed the selected
// message had left the viewport, and scrolled straight back to it, so the
// view returned to where it started for as long as the wheel was turned.
func TestWheelScrollUpKeepsMovingTheViewport(t *testing.T) {
	t.Parallel()

	u := wheelTestUI(t)
	start, _ := u.chat.list.VisibleItemIndices()

	prev := start
	for range 5 {
		wheel(u, -3)
		top, _ := u.chat.list.VisibleItemIndices()
		require.Less(t, top, prev, "every wheel tick must leave the viewport further up than the last")
		prev = top
	}
	require.False(t, u.chat.AtBottom(), "five ticks up must not end up back at the bottom")
}

// TestWheelScrollUpAdoptsTheNearestVisibleMessage: the selection follows the
// viewport rather than dragging it back, so it lands on a message that is
// actually on screen.
func TestWheelScrollUpAdoptsTheNearestVisibleMessage(t *testing.T) {
	t.Parallel()

	u := wheelTestUI(t)
	selectedBefore := u.chat.list.Selected()

	wheel(u, -3)

	require.NotEqual(t, selectedBefore, u.chat.list.Selected(),
		"the selected message scrolled out of sight, so the selection must move")
	require.True(t, u.chat.SelectedItemInView(), "the selection must be on screen after the tick")
}

// TestWheelScrollDownReturnsToTheBottom: scrolling back down still ends
// pinned to the bottom with the newest message selected, which is what the
// transcript follows.
func TestWheelScrollDownReturnsToTheBottom(t *testing.T) {
	t.Parallel()

	u := wheelTestUI(t)
	for range 5 {
		wheel(u, -3)
	}
	require.False(t, u.chat.AtBottom())

	for range 20 {
		wheel(u, 3)
	}
	require.True(t, u.chat.AtBottom(), "wheeling down must reach the bottom again")
	require.True(t, u.chat.SelectedItemInView())
}
