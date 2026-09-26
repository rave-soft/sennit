package appws

import (
	"testing"

	"github.com/rave-soft/sennit/internal/agent/notify"
	mcptools "github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
	"github.com/stretchr/testify/require"
)

// TestTranslateEvent_OutputsAreRegistered feeds every event shape
// app.setupEvents/app.ForwardEvents can hand translateEvent through it,
// and requires the result to be either nil (no frontend consumer -- see
// translateEvent's doc comment) or a type wsrpc's event registry
// (internal/workspace/wsrpc/events.go) knows about. A translateEvent
// change that starts forwarding a new, unregistered type fails here
// instead of first panicking inside Loopback's Subscribe wrapper
// (loopback_manual.go's codecEvent) or, worse, only once it reaches a
// real wire transport.
//
// pubsub.Event[thread.Event] is deliberately not a case here:
// translateEvent's thread branch calls w.threadManager(), which
// dereferences w.app -- exercising it needs a real *app.App and thread
// manager, already covered end-to-end by
// TestAppWorkspace_TranslateEvent_ThreadLifecycle in
// threads_appworkspace_test.go, whose translated pubsub.Event[proto.Thread]
// output this same registry also has an entry for.
func TestTranslateEvent_OutputsAreRegistered(t *testing.T) {
	t.Parallel()

	w := &AppWorkspace{}

	cases := []struct {
		name    string
		input   any
		wantNil bool
	}{
		{"agent notification", pubsub.Event[notify.Notification]{Payload: notify.Notification{Type: notify.TypeAgentFinished}}, false},
		{"run complete has no frontend consumer", pubsub.Event[notify.RunComplete]{Payload: notify.RunComplete{SessionID: "sess-1"}}, true},
		{"workspace changed has no frontend consumer", pubsub.Event[app.WorkspaceChanged]{}, true},
		{"mcp state changed", pubsub.Event[mcptools.Event]{Payload: mcptools.Event{Type: mcptools.EventStateChanged}}, false},
		{"mcp tools list changed", pubsub.Event[mcptools.Event]{Payload: mcptools.Event{Type: mcptools.EventToolsListChanged}}, false},
		{"mcp prompts list changed", pubsub.Event[mcptools.Event]{Payload: mcptools.Event{Type: mcptools.EventPromptsListChanged}}, false},
		{"mcp resources list changed", pubsub.Event[mcptools.Event]{Payload: mcptools.Event{Type: mcptools.EventResourcesListChanged}}, false},
		{"mcp channel message is filtered", pubsub.Event[mcptools.Event]{Payload: mcptools.Event{Type: mcptools.EventChannelMessage}}, true},
		{"lsp state changed", pubsub.Event[app.LSPEvent]{Payload: app.LSPEvent{Type: app.LSPEventStateChanged}}, false},
		{"session passes through", pubsub.Event[session.Session]{Payload: session.Session{ID: "sess-1"}}, false},
		{"message passes through", pubsub.Event[message.Message]{Payload: message.Message{ID: "msg-1"}}, false},
		{"permission request passes through", pubsub.Event[permission.PermissionRequest]{Payload: permission.PermissionRequest{ID: "perm-1"}}, false},
		{"permission notification passes through", pubsub.Event[permission.PermissionNotification]{Payload: permission.PermissionNotification{ToolCallID: "call-1"}}, false},
		{"question request passes through", pubsub.Event[question.Request]{Payload: question.Request{ID: "batch-1"}}, false},
		{"question notification passes through", pubsub.Event[question.Notification]{Payload: question.Notification{BatchID: "batch-1"}}, false},
		{"history file passes through", pubsub.Event[history.File]{Payload: history.File{ID: "hist-1"}}, false},
		{"skills event passes through", pubsub.Event[skills.Event]{Payload: skills.Event{}}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := w.translateEvent(tc.input)
			if tc.wantNil {
				require.Nil(t, got, "expected translateEvent to filter %T to nil", tc.input)
				return
			}
			require.NotNil(t, got)
			_, err := wsrpc.EncodeEvent(got)
			require.NoError(t, err, "translateEvent(%T) produced %T, which wsrpc's event registry does not know", tc.input, got)
		})
	}
}
