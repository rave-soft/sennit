package model

import (
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/util"
	"github.com/rave-soft/braid/internal/workspace"
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
// force focus to the chat list and blur the editor. Orientation (parent
// title, sibling count) lives entirely in drawChildSessionPanel now — see
// TestEnterExitChildSession_NeverSetsStatusInfoMsg — so this only covers
// focus.
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
}

// TestEnterExitChildSession_NeverSetsStatusInfoMsg is the regression test
// for the "green bar" bug: entering/cycling a child session used to post a
// status-bar breadcrumb via SetInfoMsg(InfoTypeInfo) — but InfoTypeInfo is
// styled identically to InfoTypeSuccess (see quickstyle.go's
// s.Status.InfoIndicator = s.Status.SuccessIndicator), so it rendered as a
// full-width green bar under the new child-session panel, which already
// shows the same orientation info. Entering, cycling, and exiting a child
// session must never touch the status bar's info message.
func TestEnterExitChildSession_NeverSetsStatusInfoMsg(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	u.session = &session.Session{ID: "parent-session", Title: "My Parent Session"}
	u.editor.textarea = textarea.New()
	u.chat.AppendMessages(
		newAgentItem(u.com.Styles, "tc-1"),
		newAgentItem(u.com.Styles, "tc-2"),
	)

	require.NotNil(t, u.enterChildSession("msg1", "tc-1"))
	require.True(t, u.status.InfoMsg().IsEmpty(), "entering a child session must not post a status-bar message")

	require.NotNil(t, u.cycleChildSession(1))
	require.True(t, u.status.InfoMsg().IsEmpty(), "cycling siblings must not post a status-bar message")

	require.NotNil(t, u.exitChildSession())
	require.True(t, u.status.InfoMsg().IsEmpty())
}

// TestExitChildSessionRestoresEditorFocus: once the nav stack empties (the
// user has backed all the way out of the last child session), focus must
// return to the editor. Tab no longer offers a manual way back to
// uiFocusEditor, so exitChildSession is now the only path that restores it.
func TestExitChildSessionRestoresEditorFocus(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	u.session = &session.Session{ID: "parent-session", Title: "My Parent Session"}
	u.editor.textarea = textarea.New()
	u.chat.AppendMessages(newAgentItem(u.com.Styles, "tc-1"))

	require.NotNil(t, u.enterChildSession("msg1", "tc-1"))
	require.Equal(t, uiFocusMain, u.focus)

	cmd := u.exitChildSession()
	require.NotNil(t, cmd)

	require.Equal(t, uiFocusEditor, u.focus)
	require.False(t, u.chat.list.Focused())
	require.True(t, u.editor.textarea.Focused(), "textarea.Focus() sets focused state synchronously")
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

	handled, openContainer, messageID, toolCallID := u.chat.HandleDelayedClick(DelayedClickMsg{
		ClickID: u.chat.pendingClickID,
		ItemIdx: 0,
		X:       0,
		Y:       0,
	})
	require.True(t, handled)
	require.True(t, openContainer)
	require.Equal(t, "msg1", messageID, "must resolve the clicked item's message ID for enterChildSession")
	require.Equal(t, "tc-1", toolCallID, "must resolve the clicked item's tool-call ID for enterChildSession")
	require.False(t, currentExpanded(t, item), "click on a nested container must not toggle expansion")
}

// TestHandleDelayedClickOnNestedContainer_KeyboardSelectionElsewhere is the
// regression test for the mouse-click drill-in bug: HandleDelayedClick must
// resolve the clicked container from msg.ItemIdx, not from whatever item the
// keyboard-driven selection (list.Selected()) happens to point at. Before
// the fix, a click's messageID/toolCallID came from
// Chat.SelectedNestedToolContainer, which reads list.SelectedItem() — and
// since mouse clicks no longer move that selection (see HandleMouseDown),
// clicking a finished delegation while a different item was
// keyboard-selected silently failed to enter the child session.
func TestHandleDelayedClickOnNestedContainer_KeyboardSelectionElsewhere(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	other := chat.NewToolMessageItem(u.com.Styles, "msg0",
		message.ToolCall{ID: "tc-other", Name: "bash", Input: `{}`, Finished: true}, nil, false, nil)
	container := newAgentItem(u.com.Styles, "tc-1")
	u.chat.AppendMessages(other, container)

	// Keyboard selection sits on the unrelated first item, not the
	// delegation being clicked.
	u.chat.SetSelected(0)
	require.NotEqual(t, 1, u.chat.list.Selected())

	handled, openContainer, messageID, toolCallID := u.chat.HandleDelayedClick(DelayedClickMsg{
		ClickID: u.chat.pendingClickID,
		ItemIdx: 1,
		X:       0,
		Y:       0,
	})
	require.True(t, handled)
	require.True(t, openContainer)
	require.Equal(t, "msg1", messageID)
	require.Equal(t, "tc-1", toolCallID)
}

// TestHandleDelayedClickOnPlainToolItem is the control case: a plain
// (non-nested-container) tool item no longer has anything to expand — file
// content is not something this chat lets you page through (see
// tools.go's appendResultSummary). A click is inert: reported handled with
// openContainer false, and the item does not implement chat.Expandable at
// all, so there's no toggle to fire.
func TestHandleDelayedClickOnPlainToolItem(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	item := chat.NewToolMessageItem(u.com.Styles, "msg1",
		message.ToolCall{ID: "tc-bash", Name: "bash", Input: `{}`, Finished: false}, nil, false, nil)
	u.chat.AppendMessages(item)
	u.chat.SetSelected(0)

	_, ok := item.(chat.Expandable)
	require.False(t, ok, "a plain tool item must not implement chat.Expandable — there's nothing to expand")

	handled, openContainer, _, _ := u.chat.HandleDelayedClick(DelayedClickMsg{
		ClickID: u.chat.pendingClickID,
		ItemIdx: 0,
		X:       0,
		Y:       0,
	})
	require.True(t, handled)
	require.False(t, openContainer)
}

// drillInWorkspace only implements CreateAgentToolSessionID (which
// enterChildSession calls synchronously) on top of the nil
// workspace.Workspace embed; every other method panics if called, so a
// test using it must never exercise Draw() or the async cmd that
// loadSession returns.
type drillInWorkspace struct {
	workspace.Workspace
}

func (drillInWorkspace) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return messageID + "$$" + toolCallID
}

// TestClickOnFinishedDelegation_EntersChildSession is the end-to-end
// regression test for the drill-in-broke-by-the-green-stripe-fix bug: a
// real mouse click on a finished delegation block, driven through
// UI.Update the same way the runtime delivers it (MouseClickMsg, then the
// resulting DelayedClickMsg once the double-click window's tea.Tick
// fires), must still call enterChildSession. It broke because
// HandleDelayedClick used to resolve the clicked item via
// Chat.SelectedNestedToolContainer (which reads the keyboard-driven
// list.SelectedItem()) — and after HandleMouseDown stopped moving that
// selection on click (see the focus-stripe fix), a click landing on any
// item other than whatever was last keyboard-selected (or nothing, if
// browse mode was never entered) resolved no container at all.
//
// The chat is deliberately dense: three collapsed one-line bash calls
// precede the delegation so consecutive gaps collapse to zero (see
// list.List.gapAt) — AgentToolMessageItem opts out via AlwaysSpaced, so
// the gap right before the delegation stays put, but this still exercises
// ItemIndexAtPosition's dense-list math for the plain tool rows leading up
// to the click target instead of a click at a hand-picked offset.
func TestClickOnFinishedDelegation_EntersChildSession(t *testing.T) {
	t.Parallel()

	u := newCursorTestUI(t)
	u.com.Workspace = drillInWorkspace{}
	u.session = &session.Session{ID: "parent-session", Title: "Parent"}

	for i := range 3 {
		u.chat.AppendMessages(chat.NewToolMessageItem(u.com.Styles, "msg-plain",
			message.ToolCall{ID: "tc-plain-" + string(rune('a'+i)), Name: "bash", Input: `{}`, Finished: true}, nil, false, nil))
	}
	agentItem := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-agent", Name: "agent", Input: `{}`, Finished: true}, nil, false, nil)
	agentItem.SetMessageID("msg-agent")
	agentItem.SetResult(&message.ToolResult{ToolCallID: "tc-agent", Content: "done"})
	agentIdx := u.chat.list.Len()
	u.chat.AppendMessages(agentItem)
	u.updateLayoutAndSize()

	// Find the actual on-screen row for the delegation block via the
	// list's own geometry, rather than hand-computing gap math.
	clickY := -1
	for y := 0; y < 200; y++ {
		if idx, _ := u.chat.list.ItemIndexAtPosition(0, y); idx == agentIdx {
			clickY = y
			break
		}
	}
	require.GreaterOrEqual(t, clickY, 0, "must find a screen row for the delegation item")

	_, cmd := u.Update(tea.MouseClickMsg(tea.Mouse{
		X:      u.layout.main.Min.X,
		Y:      u.layout.main.Min.Y + clickY,
		Button: uv.MouseLeft,
	}))
	require.NotNil(t, cmd, "the click must schedule a delayed-click command")

	// Unwrap the tea.Batch the runtime would otherwise flatten for us (it
	// may carry other standing cmds — e.g. animations — alongside the
	// delayed-click tick), run each synchronously, and feed the resulting
	// DelayedClickMsg back through Update exactly like the real event loop
	// would.
	batch, ok := cmd().(tea.BatchMsg)
	require.True(t, ok, "expected a tea.Batch containing the delayed-click tick")
	var delayedMsg tea.Msg
	for _, sub := range batch {
		if sub == nil {
			continue
		}
		if msg := sub(); msg != nil {
			if _, isDelayed := msg.(DelayedClickMsg); isDelayed {
				delayedMsg = msg
				break
			}
		}
	}
	require.IsType(t, DelayedClickMsg{}, delayedMsg, "the batch must contain the delayed-click tick")

	_, _ = u.Update(delayedMsg)

	require.Len(t, u.navStack, 1, "clicking a finished delegation must push a child-session nav frame")
	require.Equal(t, "parent-session", u.navStack[0].parentSessionID)
	require.Equal(t, uiFocusMain, u.focus)
}
