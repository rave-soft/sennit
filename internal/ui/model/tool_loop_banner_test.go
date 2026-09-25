package model

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/message"
)

// TestToolLoopStopBannerAppearsLive: a turn stopped on a tool-call loop
// ends on a step holding only tool calls. That message leaves the chat
// while it streams (it has nothing of its own to show), so the update that
// finishes it with a banner has to bring it back, or the person sees the
// turn simply stop.
func TestToolLoopStopBannerAppearsLive(t *testing.T) {
	u := newCursorTestUI(t)

	toolCall := message.ToolCall{ID: "call-1", Name: "read", Input: `{}`, Finished: true}
	streaming := message.Message{
		ID:    "assistant-loop",
		Role:  message.Assistant,
		Parts: []message.ContentPart{toolCall},
	}
	u.appendSessionMessage(streaming)
	u.updateSessionMessage(streaming)
	require.Nil(t, u.chat.MessageItem("assistant-loop"),
		"a tool-call-only step is not rendered as a message of its own")

	stopped := streaming
	stopped.Parts = []message.ContentPart{
		toolCall,
		message.Finish{
			Reason:  message.FinishReasonToolLoop,
			Message: "Stopped: the model kept repeating the same tool call",
			Details: "The model called read with the same arguments and got the same result 7 times.",
			Time:    1,
		},
	}
	u.updateSessionMessage(stopped)

	item := u.chat.MessageItem("assistant-loop")
	require.NotNil(t, item, "the finished step must come back to carry its banner")
	out := ansi.Strip(item.Render(120))
	require.Contains(t, out, "STOPPED")
	require.Contains(t, out, "kept repeating the same tool call")

	u.updateSessionMessage(stopped)
	count := 0
	for _, it := range u.chat.MessageItems() {
		if it.ID() == "assistant-loop" {
			count++
		}
	}
	require.Equal(t, 1, count, "a repeated update must not add the banner twice")
}
