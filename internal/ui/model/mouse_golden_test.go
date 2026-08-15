package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/stretchr/testify/require"
)

// TestMouseGolden captures rendered-screen snapshots for the mouse-driven
// branches of UI.Update (tea.MouseClickMsg, tea.MouseMotionMsg,
// tea.MouseReleaseMsg, DelayedClickMsg) ahead of extracting them into their
// own file. Each subtest asserts, with a plain require, that the state the
// branch is responsible for actually changed before taking the snapshot —
// a snapshot alone would not catch that logic breaking, only that some
// frame changed.
func TestMouseGolden(t *testing.T) {
	t.Run("breadcrumb_hover", func(t *testing.T) {
		ws := &cmdDrivingWorkspace{agentReady: true}
		m := newCmdDrivenGoldenUI(ws)
		// A breadcrumb trail (and its Back button) only renders once the UI
		// is embedded below some root — see breadcrumbCrumbs.
		m.crumbRoot = "Golden Thread"
		m.updateLayoutAndSize()

		row := m.breadcrumbBarRow()
		require.False(t, row.Empty(), "breadcrumb bar row must have room to draw")
		plan, ok := m.planBreadcrumbBar(row)
		require.True(t, ok, "breadcrumb bar must have a plan once crumbRoot is set")
		require.False(t, m.breadcrumbHover, "hover must start false")

		x := (plan.buttonRect.Min.X + plan.buttonRect.Max.X) / 2
		y := plan.buttonRect.Min.Y
		_, cmd := m.Update(tea.MouseMotionMsg(tea.Mouse{X: x, Y: y}))
		runCmdTree(m, cmd, nil)

		require.True(t, m.breadcrumbHover, "hovering the Back button must set breadcrumbHover")
		golden.RequireEqual(t, renderCmdDrivenUI(m))
	})

	t.Run("bash_tool_expand_click", func(t *testing.T) {
		ws := &cmdDrivingWorkspace{agentReady: true}
		m := newCmdDrivenGoldenUI(ws)

		toolCall := message.ToolCall{
			ID:       "bash-golden",
			Name:     "bash",
			Input:    `{"command":"go test ./..."}`,
			Finished: true,
		}
		result := &message.ToolResult{
			ToolCallID: toolCall.ID,
			Name:       "bash",
			Content:    "line one\nline two\nline three\nline four\nline five\nline six",
		}
		item := chat.NewBashToolMessageItem(m.com.Styles, toolCall, result, false)
		m.chat.AppendMessages(item)
		m.updateLayoutAndSize()

		const renderWidth = 100
		require.NotContains(t, item.Render(renderWidth), "Click to collapse",
			"the bash body must start collapsed so the click-to-expand transition is observable")

		// The item is the only one in the chat, so it sits at the top of
		// the viewport — click its header, which HandleMouseDown maps back
		// to item index 0 (see the coordinate translation in ui.go).
		downX, downY := m.layout.main.Min.X, m.layout.main.Min.Y
		_, cmd := m.Update(tea.MouseClickMsg(tea.Mouse{X: downX, Y: downY, Button: uv.MouseLeft}))
		require.NotNil(t, cmd, "a single click on a tool item must schedule the delayed expand action")
		runCmdTree(m, cmd, nil)

		require.Contains(t, item.Render(renderWidth), "Click to collapse",
			"the delayed click must have expanded the bash tool body")
		golden.RequireEqual(t, renderCmdDrivenUI(m))
	})

	t.Run("scrollbar_drag", func(t *testing.T) {
		ws := &cmdDrivingWorkspace{agentReady: true}
		m := newCmdDrivenGoldenUI(ws)
		for i := range 200 {
			m.chat.AppendMessages(chat.NewAssistantMessageItem(m.com.Styles, &message.Message{
				ID:   "m" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "line of chat content"},
				},
			}))
		}
		m.updateLayoutAndSize()
		// The default scrollbar mode only shows the scrollbar after scroll
		// activity; trigger that directly rather than depending on a real
		// scroll gesture (see chat_scrollbar_mouse_test.go).
		m.chat.showScrollbar()
		// Draw once so Chat.Draw populates the scrollbar hit-testing fields
		// (scrollbarColX, scrollbarTrackHeight, ...) before we compute a
		// click position from them.
		renderCmdDrivenUI(m)
		require.True(t, m.chat.scrollbarShown, "the message list must overflow enough to show a scrollbar")

		colX := m.chat.scrollbarColX
		trackHeight := m.chat.scrollbarTrackHeight
		require.Positive(t, trackHeight)

		screenX := m.layout.main.Min.X + colX
		topY := m.layout.main.Min.Y
		bottomY := m.layout.main.Min.Y + trackHeight - 1

		_, cmd := m.Update(tea.MouseClickMsg(tea.Mouse{X: screenX, Y: topY, Button: uv.MouseLeft}))
		runCmdTree(m, cmd, nil)
		require.True(t, m.chat.scrollbarDragging, "clicking the scrollbar column must start a drag")
		offsetNearTop := m.chat.list.Offset()

		_, cmd = m.Update(tea.MouseMotionMsg(tea.Mouse{X: screenX, Y: bottomY, Button: uv.MouseLeft}))
		runCmdTree(m, cmd, nil)
		offsetNearBottom := m.chat.list.Offset()
		require.Greater(t, offsetNearBottom, offsetNearTop,
			"dragging the thumb down must increase the scroll offset")

		_, cmd = m.Update(tea.MouseReleaseMsg(tea.Mouse{X: screenX, Y: bottomY, Button: uv.MouseLeft}))
		runCmdTree(m, cmd, nil)
		require.False(t, m.chat.scrollbarDragging, "releasing the mouse must end the drag")

		golden.RequireEqual(t, renderCmdDrivenUI(m))
	})
}
