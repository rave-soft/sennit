package model

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/chat"
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

// Config satisfies the workspace.ConfigAccessor DefaultCommon needs to
// pick a theme; returning nil is fine, DefaultCommon treats it the same
// as "no workspace".
func (agentSessionWorkspace) Config() *config.Config {
	return nil
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
			chat:   NewChat(com, config.ScrollbarDefault),
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

	childID := u.com.Workspace.CreateAgentToolSessionID("parent-msg", "tc-agent")
	u.handleChildSessionUpdate(u.com, session.Session{ID: childID, PromptTokens: 500, CompletionTokens: 120})

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
		u.handleChildSessionUpdate(u.com, session.Session{ID: "top-level-session", PromptTokens: 1})
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

	childID := u.com.Workspace.CreateAgentToolSessionID("parent-msg", "tc-agent")
	u.handleChildSessionUpdate(u.com, session.Session{
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
