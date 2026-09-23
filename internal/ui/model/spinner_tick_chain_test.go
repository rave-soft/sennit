package model

import (
	"context"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/spin"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/chatlist"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// spinningAssistantItem builds an assistant message item in the state
// isSpinning() answers true for: thinking, with no content, no tool calls
// and no finish part.
func spinningAssistantItem(t *testing.T, sty *styles.Styles, id string) chat.MessageItem {
	t.Helper()
	return chat.NewAssistantMessageItem(sty, &message.Message{
		ID:        id,
		SessionID: "s1",
		Role:      message.Assistant,
		Parts:     []message.ContentPart{message.ReasoningContent{Thinking: "still working"}},
	})
}

// TestOffscreenSpinnerKeepsItsTickChain pins that a tick which cannot be
// delivered is retried rather than dropped. A chain is self-perpetuating —
// whoever takes a StepMsg returns the command that schedules the next —
// so dropping one does not pause the spinner, it ends it: the item sits
// frozen on whatever frame it reached, and only a scroll (the single
// caller of RestartPausedVisibleAnimations) ever brings it back.
func TestOffscreenSpinnerKeepsItsTickChain(t *testing.T) {
	com := common.DefaultCommon(context.Background(), &countingWorkspace{})
	c := chatlist.NewChat(com, config.ScrollbarDefault)

	spinning := spinningAssistantItem(t, com.Styles, "m-spin")
	items := []chat.MessageItem{spinning}
	for i := range 20 {
		items = append(items, chat.NewAssistantMessageItem(com.Styles, &message.Message{
			ID:        "f" + strconv.Itoa(i),
			SessionID: "s1",
			Role:      message.Assistant,
			Parts:     []message.ContentPart{message.TextContent{Text: strings.Repeat("filler ", 20)}},
		}))
	}
	c.SetMessages(items...)
	c.SetSize(120, 20)

	start := spinning.(chat.Animatable).StartAnimation()
	require.NotNil(t, start, "a spinning item must arm an animation")
	step, ok := start().(spin.StepMsg)
	require.True(t, ok, "arming must schedule a step")

	// The chat is pinned to the bottom, so the first item is off-screen.
	first, _ := c.VisibleItemIndices()
	require.Positive(t, first, "the spinning item must be scrolled out for this test")

	next := c.Animate(step)
	require.NotNil(t, next, "an undeliverable tick must be retried, not dropped")
	require.Equal(t, step, next(), "the retry must carry the same id and generation")
}

// TestSpinnerTickSurvivesANonChatScreen is the same invariant one layer
// up: a tick that arrives while another screen is showing must come back,
// not end the chain — and must stop once the item it drives is done.
func TestSpinnerTickSurvivesANonChatScreen(t *testing.T) {
	m := newBusyUI(&countingWorkspace{})
	m.updateLayoutAndSize()
	spinning := spinningAssistantItem(t, m.com.Styles, "m-spin")
	m.chat.SetMessages(spinning)
	m.state = uiLanding

	step := spin.StepMsg{ID: "m-spin", Gen: 3}
	_, cmd := m.Update(step)
	require.NotNil(t, cmd, "a tick arriving off the chat screen must be retried")
	require.Equal(t, step, drainTo[spin.StepMsg](t, cmd), "the retry must carry the same id and generation")

	// A tick for something this chat no longer shows has nothing left to
	// drive, and must not be retried for the rest of the session.
	_, cmd = m.Update(spin.StepMsg{ID: "gone", Gen: 3})
	require.Zero(t, drainTo[spin.StepMsg](t, cmd), "a tick with no item behind it must not be retried")
}

// TestOffscreenDelegationRetriesOnlyItsRunningNestedTools pins that a
// nested tool's tick chain is kept alive by the tool itself, not by the
// delegation holding it. Nested ids map to the container's row, and a
// running delegation is unfinished for its whole lifetime, so answering
// for the container kept every nested tool that had ever spun retrying
// twice a second until the delegation ended: a long delegation scrolled
// off-screen accumulated hundreds of chains and a full re-layout per tick.
func TestOffscreenDelegationRetriesOnlyItsRunningNestedTools(t *testing.T) {
	com := common.DefaultCommon(context.Background(), &countingWorkspace{})
	c := chatlist.NewChat(com, config.ScrollbarDefault)
	sty := com.Styles

	nestedTool := func(id string, done bool) chat.ToolMessageItem {
		var res *message.ToolResult
		if done {
			res = &message.ToolResult{ToolCallID: id, Content: "ok"}
		}
		return chat.NewToolMessageItem(sty, "m-"+id,
			message.ToolCall{ID: id, Name: "bash", Input: `{"command":"true"}`, Finished: true}, res, false, nil)
	}

	inner := chat.NewAgentToolMessageItem(sty,
		message.ToolCall{ID: "tc-inner", Name: "agent", Input: `{"prompt":"inner"}`}, nil, false, nil)
	inner.AddNestedTool(nestedTool("c-deep-done", true))
	inner.AddNestedTool(nestedTool("c-deep-running", false))

	outer := chat.NewAgentToolMessageItem(sty,
		message.ToolCall{ID: "tc-outer", Name: "agent", Input: `{"prompt":"outer"}`}, nil, false, nil)
	outer.AddNestedTool(nestedTool("c-done", true))
	outer.AddNestedTool(nestedTool("c-running", false))
	outer.AddNestedTool(inner)

	items := []chat.MessageItem{outer}
	for i := range 20 {
		items = append(items, chat.NewAssistantMessageItem(sty, &message.Message{
			ID:        "f" + strconv.Itoa(i),
			SessionID: "s1",
			Role:      message.Assistant,
			Parts:     []message.ContentPart{message.TextContent{Text: strings.Repeat("filler ", 20)}},
		}))
	}
	c.SetMessages(items...)
	c.SetSize(120, 20)
	first, _ := c.VisibleItemIndices()
	require.Positive(t, first, "the delegation must be scrolled out for this test")

	for _, id := range []string{"tc-outer", "c-running", "tc-inner", "c-deep-running"} {
		require.True(t, c.AnimationLives(id), "%s is still running", id)
		require.NotNil(t, c.Animate(spin.StepMsg{ID: id, Gen: 1}), "%s's off-screen tick must be retried", id)
	}
	for _, id := range []string{"c-done", "c-deep-done"} {
		require.False(t, c.AnimationLives(id), "%s has its result", id)
		require.Nil(t, c.Animate(spin.StepMsg{ID: id, Gen: 1}), "%s's chain must end, not retry", id)
	}

	// The non-chat screen path asks the same question.
	m := newBusyUI(&countingWorkspace{})
	m.updateLayoutAndSize()
	m.chat.SetMessages(items...)
	m.state = uiLanding
	_, cmd := m.Update(spin.StepMsg{ID: "c-done", Gen: 1})
	require.Zero(t, drainTo[spin.StepMsg](t, cmd), "a finished nested tool must not be retried off the chat screen")

	// Once the delegation is over, a nested tool that never got a result
	// has nothing left to wait for either. The delegation releases its
	// nested tools here, so this pins the outcome rather than which of
	// AnimationLives' two guards produces it.
	outer.SetResult(&message.ToolResult{ToolCallID: "tc-outer", Content: "done"})
	require.False(t, c.AnimationLives("c-running"), "a finished delegation ends its nested chains")
}

// drainTo runs a command tree and returns the first message of type T.
func drainTo[T tea.Msg](t *testing.T, cmd tea.Cmd) T {
	t.Helper()
	var zero T
	if cmd == nil {
		return zero
	}
	switch msg := cmd().(type) {
	case T:
		return msg
	case tea.BatchMsg:
		for _, c := range msg {
			if got := drainTo[T](t, c); any(got) != any(zero) {
				return got
			}
		}
	}
	return zero
}
