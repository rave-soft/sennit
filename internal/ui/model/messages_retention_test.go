package model

import (
	"runtime"
	"testing"
	"weak"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestSessionMessageItemsDoNotPinTheLoadedMessages: the items built from a
// loaded session must not keep the loaded slice alive. User and assistant
// items hold a *message.Message; pointing it into the slice kept its whole
// backing array, and with it every tool message's parts, for as long as
// the session stayed open: on a long session, a hundred megabytes of tool
// output the items had already taken what they render from.
func TestSessionMessageItemsDoNotPinTheLoadedMessages(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	msgs := []message.Message{
		{ID: "u1", Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "hi"}}},
		{ID: "a1", Role: message.Assistant, Parts: []message.ContentPart{
			message.ToolCall{ID: "tc-1", Name: "bash", Input: `{"command":"ls"}`, Finished: true},
			message.Finish{Reason: message.FinishReasonToolUse},
		}},
		{ID: "t1", Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "tc-1", Content: "out"}}},
	}
	loaded := weak.Make(&msgs[0])

	items, _ := sessionMessageItems(&sty, nil, msgs)
	require.NotEmpty(t, items)
	// Drop the test's own reference: only the items may keep msgs alive.
	msgs = nil

	runtime.GC()
	require.Nil(t, loaded.Value(), "the items must not keep the loaded slice alive")
	runtime.KeepAlive(items)
}
