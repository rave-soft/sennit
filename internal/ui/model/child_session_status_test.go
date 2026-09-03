package model

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/chatlist"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// agentSessionWorkspace is a minimal workspace.Workspace stub that
// implements only the agent-tool session ID helpers exercised by
// findNestedToolContainer / handleChildSessionUpdate, mirroring the real
// "messageID$$toolCallID" format from internal/session/session.go
// (session.service.CreateAgentToolSessionID / ParseAgentToolSessionID).
type agentSessionWorkspace struct {
	workspace.Workspace

	// busySessions holds the session IDs AgentIsSessionBusy should report
	// as generating. The child-session panel's elapsed line asks about the
	// loaded child session specifically (see childPanelElapsedText), not
	// the workspace-wide agentBusyCache, so tests need a way to answer it.
	busySessions map[string]bool
}

func (w agentSessionWorkspace) AgentIsSessionBusy(sessionID string) bool {
	return w.busySessions[sessionID]
}

// Config satisfies the config reader DefaultCommon needs to pick a theme.
// An empty, non-nil Config (rather than nil) is required so
// handleChildSessionMessage's NewToolMessageItem call can probe it for
// custom-agent tool names (config.Config.AgentOverride) without a nil
// pointer dereference.
func (agentSessionWorkspace) Config() *config.Config {
	return &config.Config{}
}

func (agentSessionWorkspace) SupportsThreads() bool { return false }

// SupportsTasks answers for the delegation list behind the panel's
// agents section; no test here drives one.
func (agentSessionWorkspace) SupportsTasks() bool { return false }

func (agentSessionWorkspace) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return messageID + "$$" + toolCallID
}

func (agentSessionWorkspace) ParseAgentToolSessionID(sessionID string) (string, string, bool) {
	parts := strings.SplitN(sessionID, "$$", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// newChildSessionTestUI builds a UI with just enough wiring — a Workspace
// stub and a live Chat — to exercise the child-session live-update path
// without any of app/session/agent's real machinery.
func newChildSessionTestUI(t *testing.T) *UI {
	t.Helper()
	com := common.DefaultCommon(context.Background(), agentSessionWorkspace{})
	return &UI{
		com: com,
		// Pinned so exitChildSessionShortcut's runtime.GOOS fallback (this
		// UI has no keyMap, so it always takes that path) renders ctrl+up
		// regardless of the host running the suite.
		goos: "linux",
		widgets: widgets{
			chat:   chatlist.NewChat(com, config.ScrollbarDefault),
			status: NewStatus(com, nil),
		},
	}
}

// TestFindNestedToolContainer covers the lookup helper factored out of
// handleChildSessionMessage: it must find a registered agent/agentic_fetch
// tool item by its tool-call ID, and return nil for anything else (missing
// ID, or a tool item that isn't a nested-tool container).
func TestFindNestedToolContainer(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	agentItem := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-agent", Name: "agent", Input: `{}`, Finished: false}, nil, false, nil)
	plainItem := chat.NewToolMessageItem(u.com.Styles, "msg1",
		message.ToolCall{ID: "tc-bash", Name: "bash", Input: `{}`, Finished: false}, nil, false, nil)
	u.chat.AppendMessages(agentItem, plainItem)

	require.Same(t, agentItem, u.findNestedToolContainer("tc-agent"))
	require.Nil(t, u.findNestedToolContainer("tc-bash"),
		"a tool item that isn't a NestedToolContainer must not be returned")
	require.Nil(t, u.findNestedToolContainer("does-not-exist"))
}

// TestFindNestedToolContainer_Nested is the regression case for a
// delegation nested inside another delegation (depth 2): idInxMap only
// maps a top-level row's own ID directly, so looking up the inner
// container's tool-call ID must resolve the outer row first and then
// descend into its NestedTools() to find the inner one — see
// registerNestedIDs in internal/ui/chatlist/chat.go and
// findNestedToolContainerIn above.
func TestFindNestedToolContainer_Nested(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	innerItem := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-inner", Name: "agent", Input: `{}`, Finished: false}, nil, false, nil)
	outerItem := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-outer", Name: "agent", Input: `{}`, Finished: false}, nil, false, nil)
	outerItem.SetNestedTools([]chat.ToolMessageItem{innerItem})
	u.chat.AppendMessages(outerItem)

	require.Same(t, outerItem, u.findNestedToolContainer("tc-outer"))
	require.Same(t, innerItem, u.findNestedToolContainer("tc-inner"),
		"a delegation nested inside another delegation must still be found by its own tool-call ID")
	require.Nil(t, u.findNestedToolContainer("does-not-exist"))
}

// TestHandleChildSessionUpdate is the regression test for the "→ tokens"
// field of the running status line: a session.Session update for a child
// agent-tool session must reach the parent AgentToolMessageItem's token
// counters and show up on re-render, while updates for unrelated or
// malformed session IDs must be silently ignored (never panic).
func TestHandleChildSessionUpdate(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	item := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-agent", Name: "agent", Input: `{}`, Finished: false}, nil, false, nil)
	u.chat.AppendMessages(item)

	childID := session.CreateAgentToolSessionID("parent-msg", "tc-agent")
	u.handleChildSessionUpdate(session.Session{ID: childID, PromptTokens: 500, CompletionTokens: 120})

	// A pending delegation's chat transcript render is now just the bare
	// stub (see TestAgentToolMessageItem_PendingRendersBareStub in
	// chat/agent_test.go) — the token total now surfaces on the session
	// panel's delegation block instead, via PanelLiveActivityProvider.
	line := ansi.Strip(item.PanelStatusLine(u.com.Styles, 120))
	require.Contains(t, line, "620 tok", "child session token totals must surface on the panel's status line")

	// A session ID that isn't an agent-tool child session (the top-level
	// session's own updates, for instance) must be ignored rather than
	// panicking on the ParseAgentToolSessionID miss.
	require.NotPanics(t, func() {
		u.handleChildSessionUpdate(session.Session{ID: "top-level-session", PromptTokens: 1})
	})
}

// TestHandleChildSessionUpdate_Todos is the todos counterpart of
// TestHandleChildSessionUpdate: a session.Session update for a child
// agent-tool session must reach the parent AgentToolMessageItem's todo
// list and show up on re-render — the todos tool (domain/agent/tools/todos.go)
// saves the child session with Todos set, publishing the same
// pubsub.Event[session.Session] this handler already consumes for tokens.
func TestHandleChildSessionUpdate_Todos(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	item := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-agent", Name: "agent", Input: `{}`, Finished: false}, nil, false, nil)
	u.chat.AppendMessages(item)

	childID := session.CreateAgentToolSessionID("parent-msg", "tc-agent")
	u.handleChildSessionUpdate(session.Session{
		ID: childID,
		Todos: []session.Todo{
			{Content: "Fix the bug", Status: session.TodoStatusInProgress, ActiveForm: "Fixing the bug"},
		},
	})

	// See TestHandleChildSessionUpdate: a pending delegation's chat render
	// is just the bare stub now, so the in-progress todo's ActiveForm must
	// surface on the panel's status line instead (renderPanelStatusLine
	// prefers it over the last tool call — see currentTodoActivity).
	line := ansi.Strip(item.PanelStatusLine(u.com.Styles, 120))
	require.Contains(t, line, "Fixing the bug", "child session todos must surface on the panel's status line")
}

// TestHandleChildSessionUpdate_Depth2 is the live-update counterpart of
// TestFindNestedToolContainer_Nested: a session.Session update for a
// delegation nested two levels deep (a delegation inside a delegation)
// must reach that inner delegation's own token counters, not silently
// drop (or land on the outer one) because only the outer row's ID is
// directly registered in idInxMap.
func TestHandleChildSessionUpdate_Depth2(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	inner := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-inner", Name: "agent", Input: `{}`, Finished: false}, nil, false, nil)
	outer := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-outer", Name: "agent", Input: `{}`, Finished: false}, nil, false, nil)
	outer.SetNestedTools([]chat.ToolMessageItem{inner})
	u.chat.AppendMessages(outer)

	innerChildID := session.CreateAgentToolSessionID("outer-child-msg", "tc-inner")
	u.handleChildSessionUpdate(session.Session{ID: innerChildID, PromptTokens: 300, CompletionTokens: 40})

	innerLine := ansi.Strip(inner.PanelStatusLine(u.com.Styles, 120))
	require.Contains(t, innerLine, "340 tok",
		"a depth-2 delegation's live token totals must reach its own status line")

	outerLine := ansi.Strip(outer.PanelStatusLine(u.com.Styles, 120))
	require.NotContains(t, outerLine, "340 tok",
		"the update must land on the inner delegation, not bleed onto the outer one")
}

// TestHandleChildSessionMessage_Depth2 mirrors
// TestHandleChildSessionUpdate_Depth2 for the transcript live-update path:
// a tool-call/result event whose session ID names a delegation nested
// inside another delegation must populate that inner delegation's own
// nested tools, not be dropped by findNestedToolContainer resolving only
// the outer row and rejecting it as a non-match.
func TestHandleChildSessionMessage_Depth2(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	inner := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-inner", Name: "agent", Input: `{}`, Finished: false}, nil, false, nil)
	outer := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-outer", Name: "agent", Input: `{}`, Finished: false}, nil, false, nil)
	outer.SetNestedTools([]chat.ToolMessageItem{inner})
	u.chat.AppendMessages(outer)

	innerChildID := session.CreateAgentToolSessionID("outer-child-msg", "tc-inner")
	grandchildToolCall := message.ToolCall{ID: "tc-grandchild", Name: "bash", Input: `{}`, Finished: true}
	event := pubsub.Event[message.Message]{
		Type: pubsub.CreatedEvent,
		Payload: message.Message{
			ID:        "grandchild-msg",
			SessionID: innerChildID,
			Parts:     []message.ContentPart{grandchildToolCall},
		},
	}

	u.handleChildSessionMessage(u.com, event)

	require.Len(t, inner.NestedTools(), 1,
		"the depth-3 tool call must be attached to the depth-2 delegation that actually owns it")
	require.Equal(t, "tc-grandchild", inner.NestedTools()[0].ToolCall().ID)
	require.Len(t, outer.NestedTools(), 1,
		"the outer delegation's own nested tools (just inner) must be untouched")
	require.Same(t, chat.ToolMessageItem(inner), outer.NestedTools()[0],
		"the depth-3 tool call must not be attached to the outer delegation")
}
