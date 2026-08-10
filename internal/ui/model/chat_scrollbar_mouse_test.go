package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/stretchr/testify/require"
)

// scrollbarOverflowUI builds a chat UI with enough single-line messages that
// the list overflows its viewport, then draws once so Chat.Draw populates
// the scrollbar hit-testing fields (scrollbarShown, scrollbarColX, etc.).
func scrollbarOverflowUI(t *testing.T) *UI {
	t.Helper()

	u := newCursorTestUI(t)
	for i := range 200 {
		u.chat.AppendMessages(chat.NewAssistantMessageItem(u.com.Styles, &message.Message{
			ID:   "m" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "line of chat content"},
			},
		}))
	}
	u.updateLayoutAndSize()
	// The default scrollbar mode only shows the scrollbar after scroll
	// activity (see Chat.showScrollbar); trigger that directly rather than
	// depending on a real scroll gesture.
	u.chat.showScrollbar()
	u.drawForCursor()

	require.True(t, u.chat.scrollbarShown, "the message list must overflow enough to show a scrollbar")
	return u
}

// TestScrollbarDrag_MovesOffsetProportionally is the core regression test
// for scrollbar drag-to-scroll: dragging the thumb from near the top of the
// track to near the bottom should move the list's scroll offset from near
// zero to near its maximum, tracking the cursor rather than jumping by a
// fixed amount.
func TestScrollbarDrag_MovesOffsetProportionally(t *testing.T) {
	t.Parallel()

	u := scrollbarOverflowUI(t)

	colX := u.chat.scrollbarColX
	trackHeight := u.chat.scrollbarTrackHeight
	require.Positive(t, trackHeight)

	screenX := u.layout.main.Min.X + colX
	topY := u.layout.main.Min.Y
	bottomY := u.layout.main.Min.Y + trackHeight - 1

	_, _ = u.Update(tea.MouseClickMsg(tea.Mouse{X: screenX, Y: topY, Button: uv.MouseLeft}))
	require.True(t, u.chat.scrollbarDragging, "clicking the scrollbar column must start a drag")

	offsetNearTop := u.chat.list.Offset()

	_, _ = u.Update(tea.MouseMotionMsg(tea.Mouse{X: screenX, Y: bottomY, Button: uv.MouseLeft}))
	offsetNearBottom := u.chat.list.Offset()

	maxOffset := u.chat.list.TotalHeight() - 1 - u.chat.list.Height()

	require.Greater(t, offsetNearBottom, offsetNearTop,
		"dragging the thumb down must increase the scroll offset")
	require.InDelta(t, maxOffset, offsetNearBottom, float64(max(2, maxOffset/20)),
		"dragging to near the bottom of the track must scroll close to the end of the content")

	_, _ = u.Update(tea.MouseReleaseMsg(tea.Mouse{X: screenX, Y: bottomY, Button: uv.MouseLeft}))
	require.False(t, u.chat.scrollbarDragging, "releasing the mouse must end the drag")
}

// TestHandleScrollbarMouseUp_EndsDrag pins the direct Chat-level contract:
// HandleScrollbarMouseUp ends an in-progress drag, and any further drag
// events are then ignored until a new mouse-down starts one.
func TestHandleScrollbarMouseUp_EndsDrag(t *testing.T) {
	t.Parallel()

	u := scrollbarOverflowUI(t)

	colX := u.chat.scrollbarColX
	handled, _ := u.chat.HandleScrollbarMouseDown(colX, 0)
	require.True(t, handled)
	require.True(t, u.chat.scrollbarDragging)

	require.True(t, u.chat.HandleScrollbarMouseUp())
	require.False(t, u.chat.scrollbarDragging)

	// A subsequent up call has nothing to end.
	require.False(t, u.chat.HandleScrollbarMouseUp())

	handled, cmd := u.chat.HandleScrollbarMouseDrag(colX, 5)
	require.False(t, handled, "a drag call after mouse-up must be a no-op")
	require.Nil(t, cmd)
}

// TestTextSelection_UnaffectedByScrollbarDrag is a regression test proving
// the new scrollbar-drag path (which now runs first in ui.go's mouse
// routing) did not change the existing text-selection behavior for clicks
// that land on message content rather than the scrollbar column.
func TestTextSelection_UnaffectedByScrollbarDrag(t *testing.T) {
	t.Parallel()

	u := newCursorTestUI(t)
	u.chat.AppendMessages(
		chat.NewAssistantMessageItem(u.com.Styles, &message.Message{
			ID:   "m1",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "first message"},
			},
		}),
		chat.NewAssistantMessageItem(u.com.Styles, &message.Message{
			ID:   "m2",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "second message"},
			},
		}),
	)
	u.updateLayoutAndSize()

	downX := u.layout.main.Min.X
	downY := u.layout.main.Min.Y
	dragX := u.layout.main.Min.X + 5

	_, _ = u.Update(tea.MouseClickMsg(tea.Mouse{X: downX, Y: downY, Button: uv.MouseLeft}))
	_, _ = u.Update(tea.MouseMotionMsg(tea.Mouse{X: dragX, Y: downY, Button: uv.MouseLeft}))
	_, _ = u.Update(tea.MouseReleaseMsg(tea.Mouse{X: dragX, Y: downY, Button: uv.MouseLeft}))

	require.True(t, u.chat.HasHighlight(), "dragging across message text must still produce a text selection")
	require.False(t, u.chat.scrollbarDragging, "a text-selection drag must never start a scrollbar drag")
}

// TestScrollbarDrag_DoesNotMoveFocus pins the "mouse never changes focus"
// invariant (see uiFocusState's doc comment and the analogous tests in
// cursor_focus_test.go) for the new scrollbar-drag path specifically: none
// of the Handle scrollbar methods call Focus()/Blur(), so a full mouse
// down/drag/up sequence on the scrollbar must leave m.focus untouched.
func TestScrollbarDrag_DoesNotMoveFocus(t *testing.T) {
	t.Parallel()

	u := scrollbarOverflowUI(t)
	u.focus = uiFocusEditor
	u.editor.textarea.Focus()

	colX := u.chat.scrollbarColX
	trackHeight := u.chat.scrollbarTrackHeight
	screenX := u.layout.main.Min.X + colX
	topY := u.layout.main.Min.Y
	bottomY := u.layout.main.Min.Y + trackHeight - 1

	_, _ = u.Update(tea.MouseClickMsg(tea.Mouse{X: screenX, Y: topY, Button: uv.MouseLeft}))
	_, _ = u.Update(tea.MouseMotionMsg(tea.Mouse{X: screenX, Y: bottomY, Button: uv.MouseLeft}))
	_, _ = u.Update(tea.MouseReleaseMsg(tea.Mouse{X: screenX, Y: bottomY, Button: uv.MouseLeft}))

	require.Equal(t, uiFocusEditor, u.focus, "dragging the scrollbar must never move focus")
	require.True(t, u.editor.textarea.Focused(), "the editor must stay focused across a scrollbar drag")
}
