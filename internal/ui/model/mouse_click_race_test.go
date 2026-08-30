package model

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/chatlist"
	"github.com/stretchr/testify/require"
)

// TestMouseReleaseCopyTick_UsesClickTimeAtReleaseTime covers the
// off-goroutine access bug in the mouse-release branch of updateMouse: the
// tea.Tick closure it schedules must decide whether to fire
// copyChatHighlightMsg using the click time that was current when the
// release was handled, not m.lastClickTime read again whenever the ticker
// actually fires (which races with Update on the mouse-down path that
// keeps reassigning it).
func TestMouseReleaseCopyTick_UsesClickTimeAtReleaseTime(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("timing-based; see racecheck_on_test.go")
	}
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

	downX := u.lay.layout.main.Min.X
	downY := u.lay.layout.main.Min.Y
	dragX := downX + 5

	_, _ = u.Update(tea.MouseClickMsg(tea.Mouse{X: downX, Y: downY, Button: uv.MouseLeft}))
	_, _ = u.Update(tea.MouseMotionMsg(tea.Mouse{X: dragX, Y: downY, Button: uv.MouseLeft}))

	// Call updateMouse directly (rather than through Update, which would
	// wrap the cmd in a tea.Batch) so the raw tea.Tick cmd it schedules can
	// be inspected and run by hand.
	cmds, _ := u.updateMouse(tea.MouseReleaseMsg(tea.Mouse{X: dragX, Y: downY, Button: uv.MouseLeft}), nil)
	require.True(t, u.chat.HasHighlight(), "dragging across message text must produce a text selection")
	require.Len(t, cmds, 1, "a highlighted release must schedule exactly one delayed-copy tick")

	// While the tick is pending, simulate a fresh click overwriting
	// m.lastClickTime shortly before the tick fires -- exactly what a real
	// double-click's mouse-down handler does. The delayed action must
	// still resolve against the click time at release, not this later one.
	go func() {
		time.Sleep(chatlist.DoubleClickThreshold - 100*time.Millisecond)
		u.lastClickTime = time.Now()
	}()

	msg := cmds[0]()
	require.IsType(t, copyChatHighlightMsg{}, msg,
		"the tick must fire using the click time snapshotted at release, not a later overwrite of m.lastClickTime")
}
