package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/chat"
)

// fillChat puts enough messages in the chat that it can actually scroll.
func fillChat(t *testing.T, m *UI, n int) {
	t.Helper()
	items := make([]chat.MessageItem, 0, n)
	for i := range n {
		items = append(items, chat.NewAssistantMessageItem(m.com.Styles, &message.Message{
			ID:        "m" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			SessionID: "s1",
			Role:      message.Assistant,
			Parts:     []message.ContentPart{message.TextContent{Text: "line of text to fill the viewport"}},
		}))
	}
	m.chat.SetMessages(items...)
}

// TestHoverDoesNotScrollTheChat pins the edge auto-scroll against the
// mouse mode it actually runs under. The main screen asks for
// MouseModeAllMotion (hover feedback needs it), so a motion event arrives
// for every pointer movement, button or no button. Ungated, each one ran
// the edge-scroll branch: moving the pointer over the lower part of the
// window scrolled the conversation and snapped it to the selection,
// without anything being dragged.
func TestHoverDoesNotScrollTheChat(t *testing.T) {
	m := newBusyUI(&countingWorkspace{})
	m.updateLayoutAndSize()
	fillChat(t, m, 60)
	m.chat.ScrollToTop()

	before := m.chat.list.Offset()
	require.False(t, m.chat.Dragging(), "nothing is being dragged in this test")

	// A pointer sweep along the bottom of the window, no button held.
	for range 5 {
		m.Update(tea.MouseMotionMsg{X: 20, Y: m.lay.height - 1})
	}

	require.Equal(t, before, m.chat.list.Offset(), "hovering must not scroll the chat")
}
