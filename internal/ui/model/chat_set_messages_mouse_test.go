package model

import (
	"testing"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/stretchr/testify/require"
)

// TestSetMessagesInvalidatesPendingDelayedClick covers the stale-click bug:
// a single click schedules a DelayedClickMsg (HandleMouseDown) that resolves
// against whatever item is at the clicked index once the double-click
// window elapses. If the session is switched (SetMessages loads a new
// transcript) before that timer fires, the delayed click must not resolve
// against the new session's item at that same index -- SetMessages has to
// invalidate any click pending from the previous session.
func TestSetMessagesInvalidatesPendingDelayedClick(t *testing.T) {
	t.Parallel()

	u := newCursorTestUI(t)
	u.chat.AppendMessages(chat.NewAssistantMessageItem(u.com.Styles, &message.Message{
		ID:   "m1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "first message"},
		},
	}))
	u.updateLayoutAndSize()

	handled, cmd := u.chat.HandleMouseDown(0, 0)
	require.True(t, handled, "mouse down on the message must register a pending click")
	require.NotNil(t, cmd, "a single click must schedule a delayed action")
	clickID := u.chat.pendingClickID

	// The user switches sessions before the delayed-click timer fires.
	newItem := chat.NewAssistantMessageItem(u.com.Styles, &message.Message{
		ID:   "m2-other-session",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			// A thinking block makes the item's whole first line
			// mouse-clickable (see AssistantMessageItem.HandleMouseClick),
			// so a stale DelayedClickMsg landing on it is actually
			// actionable rather than silently ignored for unrelated
			// reasons.
			message.ReasoningContent{Thinking: "a different session's thinking"},
		},
	})
	u.chat.SetMessages(newItem)

	// SetMessages must invalidate the click pending from the previous
	// session's item — the same mechanism ClearMouse uses.
	require.NotEqual(t, clickID, u.chat.pendingClickID,
		"SetMessages must invalidate any click pending from the previous session")

	handledDelayed, _, _, _ := u.chat.HandleDelayedClick(DelayedClickMsg{ClickID: clickID, ItemIdx: 0, X: 0, Y: 0})
	require.False(t, handledDelayed,
		"a click pending from the previous session must not resolve against the newly loaded session's items")
}
