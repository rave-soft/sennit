package model

import (
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/stretchr/testify/require"
)

func TestTaskStatesByToolCall(t *testing.T) {
	t.Parallel()

	states := taskStatesByToolCall([]proto.Thread{
		{SessionID: session.CreateAgentToolSessionID("m1", "tc-done"), Status: string(proto.ThreadStatusCompleted)},
		{SessionID: session.CreateAgentToolSessionID("m2", "tc-running"), Status: string(proto.ThreadStatusRunning)},
		{SessionID: session.CreateAgentToolSessionID("m3", "tc-idle"), Status: string(proto.ThreadStatusIdle)},
		{SessionID: "not-a-delegation-session", Status: string(proto.ThreadStatusCompleted)},
	})

	require.Equal(t, map[string]bool{"tc-done": true, "tc-running": false}, states)
}

// TestLoadNestedToolCallsSkipsFinishedBackgroundDispatch is the case the
// regression session actually had: 160 of its 175 delegations were
// background dispatches, 159 of them long finished, and each one's child
// transcript was read on open because the acknowledgement alone looked like
// work still running.
func TestLoadNestedToolCallsSkipsFinishedBackgroundDispatch(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	newDispatch := func(toolCallID, messageID, taskID string) *chat.AgentToolMessageItem {
		item := chat.NewAgentToolMessageItem(u.com.Styles,
			message.ToolCall{ID: toolCallID, Name: "agent", Input: `{}`, Finished: true},
			&message.ToolResult{ToolCallID: toolCallID, Content: "dispatched", Metadata: `{"task_id":"` + taskID + `"}`},
			false, nil)
		item.SetMessageID(messageID)
		return item
	}
	items := []chat.MessageItem{
		newDispatch("tc-finished", "msg-finished", "task-1"),
		newDispatch("tc-running", "msg-running", "task-2"),
		newDispatch("tc-unknown", "msg-unknown", "task-3"),
	}
	markBackgroundDelegations(items, map[string]bool{"tc-finished": true, "tc-running": false})

	recorder := &nestedLoadRecorder{}
	require.NoError(t, loadNestedToolCalls(t.Context(), recorder, u.com.Styles, &config.Config{}, "root", 1, items))

	require.Equal(t, []string{
		session.CreateAgentToolSessionID("msg-running", "tc-running"),
		session.CreateAgentToolSessionID("msg-unknown", "tc-unknown"),
	}, recorder.requested, "only a dispatch whose task is known to be finished skips its transcript")
}

// TestRefreshDelegationBlocksMarksFinishedBackgroundDispatch covers the live
// path: a task finishing while its session is open releases the block.
func TestRefreshDelegationBlocksMarksFinishedBackgroundDispatch(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	item := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-bg", Name: "agent", Input: `{}`, Finished: true},
		&message.ToolResult{ToolCallID: "tc-bg", Content: "dispatched", Metadata: `{"task_id":"task-1"}`},
		false, nil)
	item.SetMessageID("msg-bg")
	u.chat.AppendMessages(item)

	u.chat.SetBackgroundDelegationsDone(taskStatesByToolCall([]proto.Thread{
		{SessionID: session.CreateAgentToolSessionID("msg-bg", "tc-bg"), Status: string(proto.ThreadStatusCompleted)},
	}))
	require.True(t, item.NestedToolsReleased())

	u.chat.SetBackgroundDelegationsDone(nil)
	require.True(t, item.NestedToolsReleased(), "no record of the task is no reason to revive the block")
}
