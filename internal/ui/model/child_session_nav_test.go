package model

import (
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/attachments"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// newAgentItem builds an agent-tool chat item (a NestedToolContainer) with
// the given message/tool-call IDs, mirroring how the real chat populates
// SetMessageID after construction (see NewAgentToolMessageItem).
func newAgentItem(sty *styles.Styles, toolCallID string) *chat.AgentToolMessageItem {
	// All nav tests share one parent message; the helper hardcodes its ID.
	item := chat.NewAgentToolMessageItem(sty,
		message.ToolCall{ID: toolCallID, Name: "agent", Input: `{}`, Finished: false}, nil, false, nil)
	item.SetMessageID("msg1")
	return item
}

// TestEnterChildSessionPushesFrame: entering a child session from a
// selected agent-tool item must push a sessionNavFrame with the parent ID,
// the full sibling list (in chat order), and the correct index of the
// entered delegation among its siblings.
func TestEnterChildSessionPushesFrame(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	u.sess.current = &session.Session{ID: "parent-session"}
	u.chat.AppendMessages(
		newAgentItem(u.com.Styles, "tc-1"),
		newAgentItem(u.com.Styles, "tc-2"),
	)

	cmd := u.enterChildSession("msg1", "tc-2")
	require.NotNil(t, cmd)
	require.Len(t, u.sess.navStack, 1)

	frame := u.sess.navStack[0]
	require.Equal(t, "parent-session", frame.parentSessionID)
	// Only the identity is pinned here; the display data each ref also
	// carries (label, timing — see childSessionRef) is covered by
	// TestCycleChildSessionKeepsSiblingTiming.
	require.Len(t, frame.siblings, 2)
	require.Equal(t, "msg1", frame.siblings[0].messageID)
	require.Equal(t, "tc-1", frame.siblings[0].toolCallID)
	require.Equal(t, "msg1", frame.siblings[1].messageID)
	require.Equal(t, "tc-2", frame.siblings[1].toolCallID)
	require.Equal(t, 1, frame.siblingIndex, "tc-2 is the second sibling in chat order")
}

// TestCycleChildSessionKeepsSiblingTiming: cycling to a sibling happens
// after navigation has descended, so the parent's chat items are gone by
// then. The sibling's label and elapsed-time origin must come from the
// snapshot taken when the frame was pushed — looking them up in the
// now-loaded child session finds nothing and used to leave the panel
// describing a nameless delegation that had just started.
func TestCycleChildSessionKeepsSiblingTiming(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	u.sess.current = &session.Session{ID: "parent-session"}

	second := chat.NewAgentToolMessageItem(u.com.Styles,
		message.ToolCall{ID: "tc-2", Name: "agent", Input: `{"prompt":"review the parser"}`}, nil, false, nil)
	second.SetMessageID("msg1")
	start := time.Now().Add(-12 * time.Minute)
	second.RestoreTiming(start, time.Time{})
	u.chat.AppendMessages(newAgentItem(u.com.Styles, "tc-1"), second)

	require.NotNil(t, u.enterChildSession("msg1", "tc-1"))
	// Standing in the child session now: its own items replaced the
	// parent's, which is what makes the sibling unlookupable.
	u.chat.SetMessages()

	require.NotNil(t, u.cycleChildSession(1))
	frame := u.sess.navStack[0]
	require.Equal(t, 1, frame.siblingIndex)
	require.Equal(t, "review the parser", frame.label)
	require.WithinDuration(t, start, frame.delegationStart, time.Second,
		"the sibling's elapsed time must continue from when it started, not from the moment it was cycled to")
}

// TestCycleChildSessionWrapsAndReplacesTopFrame: with multiple sibling
// delegations loaded, ctrl+left/ctrl+right must wrap the sibling index and
// update the existing top frame in place rather than pushing a new one.
func TestCycleChildSessionWrapsAndReplacesTopFrame(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	u.sess.current = &session.Session{ID: "parent-session"}
	u.chat.AppendMessages(
		newAgentItem(u.com.Styles, "tc-1"),
		newAgentItem(u.com.Styles, "tc-2"),
		newAgentItem(u.com.Styles, "tc-3"),
	)

	require.NotNil(t, u.enterChildSession("msg1", "tc-1"))
	require.Len(t, u.sess.navStack, 1)
	require.Equal(t, 0, u.sess.navStack[0].siblingIndex)

	cmd := u.cycleChildSession(1)
	require.NotNil(t, cmd)
	require.Len(t, u.sess.navStack, 1, "cycling must not push a new frame")
	require.Equal(t, 1, u.sess.navStack[0].siblingIndex)

	cmd = u.cycleChildSession(1)
	require.NotNil(t, cmd)
	require.Equal(t, 2, u.sess.navStack[0].siblingIndex)

	// Wraps from the last sibling back to the first.
	cmd = u.cycleChildSession(1)
	require.NotNil(t, cmd)
	require.Equal(t, 0, u.sess.navStack[0].siblingIndex)

	// And the other direction wraps back to the last.
	cmd = u.cycleChildSession(-1)
	require.NotNil(t, cmd)
	require.Equal(t, 2, u.sess.navStack[0].siblingIndex)
}

// TestCycleChildSessionNoOpWithoutFrameOrSiblings covers the two no-op
// paths: an empty nav stack, and a frame with only one sibling (nothing to
// cycle to).
func TestCycleChildSessionNoOpWithoutFrameOrSiblings(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	require.Nil(t, u.cycleChildSession(1), "no active frame")

	u.sess.current = &session.Session{ID: "parent-session"}
	u.chat.AppendMessages(newAgentItem(u.com.Styles, "tc-1"))
	require.NotNil(t, u.enterChildSession("msg1", "tc-1"))
	require.Nil(t, u.cycleChildSession(1), "only one sibling, nothing to cycle to")
	require.Len(t, u.sess.navStack, 1)
	require.Equal(t, 0, u.sess.navStack[0].siblingIndex)
}

// TestExitChildSessionPopsStack: ctrl+up pops the top frame and returns a
// cmd to load its parentSessionID; on an empty stack it must no-op safely
// rather than panicking.
func TestExitChildSessionPopsStack(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	require.NotPanics(t, func() {
		cmd := u.exitChildSession()
		require.Nil(t, cmd)
	})

	u.sess.current = &session.Session{ID: "parent-session"}
	u.editor.textarea = textarea.New()
	u.chat.AppendMessages(newAgentItem(u.com.Styles, "tc-1"))
	require.NotNil(t, u.enterChildSession("msg1", "tc-1"))
	require.Len(t, u.sess.navStack, 1)

	cmd := u.exitChildSession()
	require.NotNil(t, cmd)
	require.Len(t, u.sess.navStack, 0)
}

// TestEnterChildSessionKeyNoOpOnNonNestedItem: ctrl+down must be a no-op
// (no frame pushed) when the currently selected chat item isn't a
// nested-tool container, e.g. a plain bash tool item.
func TestEnterChildSessionKeyNoOpOnNonNestedItem(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	u.sess.current = &session.Session{ID: "parent-session"}
	u.state = uiChat
	u.focus = uiFocusMain
	u.keyMap = DefaultKeyMap()
	u.dialog = dialog.NewOverlay()
	u.editor.attachments = attachments.New(nil, attachments.Keymap{})
	u.chat.AppendMessages(chat.NewToolMessageItem(u.com.Styles, "msg1",
		message.ToolCall{ID: "tc-bash", Name: "bash", Input: `{}`, Finished: false}, nil, false, nil))
	u.chat.SetSelected(0)

	_, cmd := u.Update(tea.KeyPressMsg{Mod: tea.ModAlt, Code: tea.KeyDown})
	require.Empty(t, u.sess.navStack, "selecting a non-nested-tool item must not push a nav frame")
	_ = cmd
}

// TestAltUpExitsChildSessionThroughUpdate is a regression test for a
// reported "ctrl+up does nothing while viewing a subagent session" bug:
// ExitChildSession is only matched in the uiFocusMain arm of
// handleKeyPressMsg's focus switch, so the key only does anything if
// entering a child session reliably forces m.focus there (see
// enterChildSession and uiFocusState's doc comment) and nothing upstream
// (dialogs, activeInline, the textarea) intercepts the keypress first.
// Exercised through the full Update() dispatch — not a direct
// exitChildSession() call — so it catches routing bugs, not just the
// nav-stack bookkeeping.
func TestAltUpExitsChildSessionThroughUpdate(t *testing.T) {
	t.Parallel()

	u := newChildSessionTestUI(t)
	u.sess.current = &session.Session{ID: "parent-session", Title: "Parent"}
	u.state = uiChat
	u.keyMap = DefaultKeyMap()
	u.dialog = dialog.NewOverlay()
	u.editor.attachments = attachments.New(nil, attachments.Keymap{})
	u.editor.textarea = textarea.New()
	u.chat.AppendMessages(newAgentItem(u.com.Styles, "tc-1"))

	require.NotNil(t, u.enterChildSession("msg1", "tc-1"))
	require.Len(t, u.sess.navStack, 1)
	require.Equal(t, uiFocusMain, u.focus, "entering a child session must force focus off the editor")

	_, cmd := u.Update(tea.KeyPressMsg{Mod: tea.ModAlt, Code: tea.KeyUp})

	require.Empty(t, u.sess.navStack, "ctrl+up must pop the nav stack through the normal key-routing path")
	require.NotNil(t, cmd, "must return the loadSession cmd for the parent")
}

// TestNewSessionClearsChildSessionNav is a regression test: newSession
// reset a dozen pieces of session state but left navStack alone, so
// starting a new session (ctrl+n) while drilled into a delegation left
// viewingChildSession() true — sendMessage kept refusing with "viewing
// subagent session" on the freshly created session, with no key binding
// left to escape it (alt+up is only bound in uiFocusMain, and ctrl+n moves
// focus to the editor).
func TestNewSessionClearsChildSessionNav(t *testing.T) {
	t.Parallel()

	u := newBusyUI(&countingWorkspace{})
	u.sess.navStack = []sessionNavFrame{{parentSessionID: "parent-session", childSessionID: "child-1"}}
	u.focus = uiFocusMain

	cmd := u.newSession()

	require.NotNil(t, cmd)
	require.False(t, u.viewingChildSession(), "newSession must drop a stale nav stack")
	require.Equal(t, uiFocusEditor, u.focus)
}

// TestSelectSessionClearsChildSessionNav is a regression test: picking a
// session from the ctrl+s dialog while drilled into a delegation went
// straight to requestSessionLoad without touching navStack, so the newly
// loaded session inherited "viewing subagent session" from whatever was
// drilled into before — same symptom as TestNewSessionClearsChildSessionNav,
// different entry point.
func TestSelectSessionClearsChildSessionNav(t *testing.T) {
	t.Parallel()

	u := newBusyUI(&countingWorkspace{})
	u.sess.navStack = []sessionNavFrame{{parentSessionID: "parent-session", childSessionID: "child-1"}}
	u.focus = uiFocusMain

	cmd, handled := u.applySessionDialogAction(dialog.ActionSelectSession{Session: session.Session{ID: "s2"}})

	require.True(t, handled)
	require.NotNil(t, cmd)
	require.False(t, u.viewingChildSession(), "selecting a session must drop a stale nav stack")
	require.Equal(t, uiFocusEditor, u.focus)
}
