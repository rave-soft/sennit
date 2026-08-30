package model

import (
	"testing"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/chatlist"
	"github.com/stretchr/testify/require"
)

// TestRemoveMessageInvalidatesPendingDelayedClick covers a regression:
// chatlist.DelayedClickMsg.ItemIdx is a raw list index captured at click time.
// RemoveMessage shifts every later index down by one, but — unlike
// SetMessages/ClearMessages — never invalidated a click pending from
// before the removal. A tool-status placeholder disappearing mid
// double-click window (a common occurrence while the agent is streaming)
// let the delayed click resolve against whatever item slid into the
// clicked item's old slot, instead of being dropped.
func TestRemoveMessageInvalidatesPendingDelayedClick(t *testing.T) {
	t.Parallel()

	u := newCursorTestUI(t)
	u.chat.AppendMessages(
		chat.NewAssistantMessageItem(u.com.Styles, &message.Message{
			ID:   "removed",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "will be removed"},
			},
		}),
		chat.NewAssistantMessageItem(u.com.Styles, &message.Message{
			ID:   "survivor",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				// A thinking block makes the item's whole first line
				// mouse-clickable (see AssistantMessageItem.HandleMouseClick),
				// so a stale chatlist.DelayedClickMsg landing on it is actually
				// actionable rather than silently ignored for unrelated
				// reasons.
				message.ReasoningContent{Thinking: "the item that slides into index 0"},
			},
		}),
	)
	u.updateLayoutAndSize()

	// Click the first row (index 0, "removed"). Its delayed action is
	// scheduled against index 0.
	handled, cmd := u.chat.HandleMouseDown(0, 0)
	require.True(t, handled, "mouse down on the message must register a pending click")
	require.NotNil(t, cmd, "a single click must schedule a delayed action")
	clickID := u.chat.PendingClickID()

	// "removed" itself is removed (e.g. a tool placeholder resolving)
	// before the double-click window elapses. "survivor" slides down into
	// index 0 — the slot the pending click still names.
	u.chat.RemoveMessage("removed")

	require.NotEqual(t, clickID, u.chat.PendingClickID(),
		"RemoveMessage must invalidate any click pending before the removal")

	// Without invalidation, this would resolve against "survivor" at
	// index 0 — an item the user never clicked.
	handledDelayed, _, _, _ := u.chat.HandleDelayedClick(chatlist.DelayedClickMsg{ClickID: clickID, ItemIdx: 0, X: 0, Y: 0})
	require.False(t, handledDelayed,
		"a click pending before a removal must not resolve against the item that shifted into its slot")
}
