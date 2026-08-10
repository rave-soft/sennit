package model

import (
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/util"
	"github.com/stretchr/testify/require"
)

// TestSendMessageBlockedWhileViewingChildSession: sendMessage must refuse
// to send (and never touch the workspace) while a child session is being
// viewed — the guard must fire before AgentReadyErr/CreateSession, which
// would panic against agentSessionWorkspace's unimplemented methods.
func TestSendMessageBlockedWhileViewingChildSession(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	u.navStack = append(u.navStack, sessionNavFrame{parentSessionID: "parent"})

	var cmd tea.Cmd
	require.NotPanics(t, func() {
		cmd = u.sendMessage("hello")
	})
	require.NotNil(t, cmd)

	msg := cmd()
	info, ok := msg.(util.InfoMsg)
	require.True(t, ok, "expected a util.InfoMsg, got %T", msg)
	require.Equal(t, util.InfoTypeWarn, info.Type)
	require.Contains(t, info.Msg, "alt+up to return")
}

// TestEnterChildSessionForcesReadOnlyFocus: entering a child session must
// force focus to the chat list, blur the editor, and post a breadcrumb
// status message naming the parent and the sibling count.
func TestEnterChildSessionForcesReadOnlyFocus(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	u.session = &session.Session{ID: "parent-session", Title: "My Parent Session"}
	u.focus = uiFocusEditor
	u.editor.textarea = textarea.New()
	u.editor.textarea.Focus()
	u.chat.AppendMessages(
		newAgentItem(u.com.Styles, "tc-1"),
		newAgentItem(u.com.Styles, "tc-2"),
	)

	cmd := u.enterChildSession("msg1", "tc-1")
	require.NotNil(t, cmd)

	require.Equal(t, uiFocusMain, u.focus)
	require.False(t, u.editor.textarea.Focused(), "editor textarea must be blurred")

	info := u.status.InfoMsg()
	require.False(t, info.IsEmpty())
	require.Contains(t, info.Msg, "My Parent Session")
	require.Contains(t, info.Msg, "(1/2)")
	require.Contains(t, info.Msg, "alt+up to return")
}

// TestExitChildSessionClearsBreadcrumb: once the nav stack empties, the
// breadcrumb status message must be cleared.
func TestExitChildSessionClearsBreadcrumb(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	u.session = &session.Session{ID: "parent-session", Title: "My Parent Session"}
	u.chat.AppendMessages(newAgentItem(u.com.Styles, "tc-1"))

	require.NotNil(t, u.enterChildSession("msg1", "tc-1"))
	require.False(t, u.status.InfoMsg().IsEmpty())

	require.NotNil(t, u.exitChildSession())
	require.True(t, u.status.InfoMsg().IsEmpty())
}

// currentExpanded reads an item's current expanded state via two
// ToggleExpanded calls that cancel out, leaving the item's state
// unchanged.
func currentExpanded(t *testing.T, item any) bool {
	t.Helper()
	exp, ok := item.(chat.Expandable)
	require.True(t, ok, "%T must implement chat.Expandable", item)
	exp.ToggleExpanded()
	return exp.ToggleExpanded()
}

// TestHandleDelayedClickOnNestedContainer: a delayed click on a nested-tool
// container (agent delegation) must be reported as handled+openContainer,
// and must NOT toggle expansion — that's the caller's job (navigate into
// the child session instead).
func TestHandleDelayedClickOnNestedContainer(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	item := newAgentItem(u.com.Styles, "tc-1")
	u.chat.AppendMessages(item)
	u.chat.SetSelected(0)

	require.False(t, currentExpanded(t, item), "fresh item starts collapsed")

	handled, openContainer := u.chat.HandleDelayedClick(DelayedClickMsg{
		ClickID: u.chat.pendingClickID,
		ItemIdx: 0,
		X:       0,
		Y:       0,
	})
	require.True(t, handled)
	require.True(t, openContainer)
	require.False(t, currentExpanded(t, item), "click on a nested container must not toggle expansion")
}

// TestHandleDelayedClickOnPlainToolItem is the control case: a plain
// (non-nested-container) tool item's click behavior is unchanged — handled
// but openContainer is false, and expansion still toggles as before.
func TestHandleDelayedClickOnPlainToolItem(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	item := chat.NewToolMessageItem(u.com.Styles, "msg1",
		message.ToolCall{ID: "tc-bash", Name: "bash", Input: `{}`, Finished: false}, nil, false, nil)
	u.chat.AppendMessages(item)
	u.chat.SetSelected(0)

	require.False(t, currentExpanded(t, item), "fresh item starts collapsed")

	handled, openContainer := u.chat.HandleDelayedClick(DelayedClickMsg{
		ClickID: u.chat.pendingClickID,
		ItemIdx: 0,
		X:       0,
		Y:       0,
	})
	require.True(t, handled)
	require.False(t, openContainer)
	require.True(t, currentExpanded(t, item), "expansion must still toggle for a plain tool item")
}
