package model

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// nestedLoadRecorder records which child sessions loadNestedToolCalls asks
// for.
type nestedLoadRecorder struct {
	workspace.SessionStore

	requested []string
}

func (r *nestedLoadRecorder) ListMessagesBySessionIDs(_ context.Context, _ string, _ uint64, sessionIDs []string) (map[string][]message.Message, error) {
	r.requested = append(r.requested, sessionIDs...)
	return map[string][]message.Message{}, nil
}

// TestLoadNestedToolCallsSkipsFinishedDelegations is the regression test for
// a long session that took gigabytes to open: it held 175 delegations, and
// opening it read all 160 child transcripts - 19k messages, 292MB of JSON -
// to fill nested tool trees that finished delegations never render. Only a
// delegation still running needs its child transcript.
func TestLoadNestedToolCallsSkipsFinishedDelegations(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	finished := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-finished", Name: "agent", Input: `{}`, Finished: true},
		&message.ToolResult{ToolCallID: "tc-finished", Content: "done"}, false, nil)
	finished.SetMessageID("msg-finished")
	running := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-running", Name: "agent", Input: `{}`, Finished: true}, nil, false, nil)
	running.SetMessageID("msg-running")

	recorder := &nestedLoadRecorder{}
	err := loadNestedToolCalls(t.Context(), recorder, u.com.Styles, &config.Config{}, "root", 1,
		[]chat.MessageItem{finished, running})
	require.NoError(t, err)

	require.Equal(t, []string{session.CreateAgentToolSessionID("msg-running", "tc-running")}, recorder.requested)
}

// TestHandleChildSessionMessageIgnoresFinishedDelegation: a child event that
// lands after its delegation finished must not rebuild the nested tools the
// delegation let go of.
func TestHandleChildSessionMessageIgnoresFinishedDelegation(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	item := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-done", Name: "agent", Input: `{}`, Finished: true},
		&message.ToolResult{ToolCallID: "tc-done", Content: "done"}, false, nil)
	u.chat.AppendMessages(item)

	u.handleChildSessionMessage(u.com, pubsub.Event[message.Message]{
		Type: pubsub.CreatedEvent,
		Payload: message.Message{
			ID:        "late-msg",
			SessionID: session.CreateAgentToolSessionID("parent-msg", "tc-done"),
			Parts:     []message.ContentPart{message.ToolCall{ID: "tc-late", Name: "bash", Input: `{}`, Finished: true}},
		},
	})

	require.Empty(t, item.NestedTools())
}
