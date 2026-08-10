package model

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/workspace"
	"github.com/stretchr/testify/require"
)

// agentSessionWorkspace is a minimal workspace.Workspace stub that
// implements only the agent-tool session ID helpers exercised by
// findNestedToolContainer / handleChildSessionUpdate, mirroring the real
// "messageID$$toolCallID" format from internal/session/session.go
// (session.service.CreateAgentToolSessionID / ParseAgentToolSessionID).
type agentSessionWorkspace struct {
	workspace.Workspace
}

// Config satisfies the workspace.ConfigAccessor DefaultCommon needs to
// pick a theme; returning nil is fine, DefaultCommon treats it the same
// as "no workspace".
func (agentSessionWorkspace) Config() *config.Config {
	return nil
}

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
		com:  com,
		chat: NewChat(com, config.ScrollbarDefault),
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
		message.ToolCall{ID: "tc-agent", Name: "agent", Input: `{}`, Finished: false}, nil, false)
	plainItem := chat.NewToolMessageItem(u.com.Styles, "msg1",
		message.ToolCall{ID: "tc-bash", Name: "bash", Input: `{}`, Finished: false}, nil, false)
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
		message.ToolCall{ID: "tc-agent", Name: "agent", Input: `{}`, Finished: false}, nil, false)
	u.chat.AppendMessages(item)

	childID := u.com.Workspace.CreateAgentToolSessionID("parent-msg", "tc-agent")
	u.handleChildSessionUpdate(session.Session{ID: childID, PromptTokens: 500, CompletionTokens: 120})

	out := ansi.Strip(item.Render(120))
	require.Contains(t, out, "620 tok", "child session token totals must surface on the parent's status line")

	// A session ID that isn't an agent-tool child session (the top-level
	// session's own updates, for instance) must be ignored rather than
	// panicking on the ParseAgentToolSessionID miss.
	require.NotPanics(t, func() {
		u.handleChildSessionUpdate(session.Session{ID: "top-level-session", PromptTokens: 1})
	})
}
