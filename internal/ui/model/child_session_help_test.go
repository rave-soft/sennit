package model

import (
	"slices"
	"testing"

	"charm.land/bubbles/v2/key"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/attachments"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// helpKeyByHelpText reports whether any binding in binds has the given
// [key.Help].Key text, e.g. "ctrl+↓".
func helpKeyByHelpText(binds []key.Binding, helpKey string) bool {
	return slices.ContainsFunc(binds, func(b key.Binding) bool {
		return b.Help().Key == helpKey
	})
}

// fullHelpKeyByHelpText flattens FullHelp's rows before checking.
func fullHelpKeyByHelpText(binds [][]key.Binding, helpKey string) bool {
	for _, row := range binds {
		if helpKeyByHelpText(row, helpKey) {
			return true
		}
	}
	return false
}

// newHelpTestUI builds a minimal UI in uiChat/uiFocusMain with a live
// session, mirroring the field set TestEnterChildSessionKeyNoOpOnNonNestedItem
// needs to keep ShortHelp/FullHelp from nil-panicking.
func newHelpTestUI(t *testing.T) *UI {
	t.Helper()
	u := newChildSessionTestUI(t)
	u.sess.session = &session.Session{ID: "parent-session"}
	u.state = uiChat
	u.focus = uiFocusMain
	u.keyMap = DefaultKeyMap()
	u.dialog = dialog.NewOverlay()
	u.editor.attachments = attachments.New(nil, attachments.Keymap{})
	return u
}

// TestShortFullHelpShowEnterChildSessionOnNestedItem: with a sub-agent
// delegation selected in uiFocusMain, both ShortHelp and FullHelp must
// surface the "enter subagent" binding.
func TestShortFullHelpShowEnterChildSessionOnNestedItem(t *testing.T) {
	t.Parallel()

	u := newHelpTestUI(t)
	u.chat.AppendMessages(newAgentItem(u.com.Styles, "tc-1"))
	u.chat.SetSelected(0)

	require.True(t, helpKeyByHelpText(u.ShortHelp(), "ctrl+↓"),
		"ShortHelp must show enter-subagent when a nested-tool item is selected")
	require.True(t, fullHelpKeyByHelpText(u.FullHelp(), "ctrl+↓"),
		"FullHelp must show enter-subagent when a nested-tool item is selected")
}

// TestShortFullHelpHideEnterChildSessionOnPlainItem: a non-container
// selection (or no session at all) must not surface enter-subproto.
func TestShortFullHelpHideEnterChildSessionOnPlainItem(t *testing.T) {
	t.Parallel()

	u := newHelpTestUI(t)
	u.chat.AppendMessages(chat.NewToolMessageItem(u.com.Styles, "msg1",
		message.ToolCall{ID: "tc-bash", Name: "bash", Input: `{}`, Finished: false}, nil, false, nil))
	u.chat.SetSelected(0)

	require.False(t, helpKeyByHelpText(u.ShortHelp(), "ctrl+↓"))
	require.False(t, fullHelpKeyByHelpText(u.FullHelp(), "ctrl+↓"))
}

// TestShortFullHelpShowAllChildSessionNavWithMultipleSiblings: viewing a
// child session with more than one sibling must show exit/prev/next.
func TestShortFullHelpShowAllChildSessionNavWithMultipleSiblings(t *testing.T) {
	t.Parallel()

	u := newHelpTestUI(t)
	u.sess.navStack = []sessionNavFrame{
		{
			parentSessionID: "parent-session",
			siblings: []childSessionRef{
				{messageID: "msg1", toolCallID: "tc-1"},
				{messageID: "msg1", toolCallID: "tc-2"},
			},
			siblingIndex: 0,
		},
	}

	for _, helpKey := range []string{"ctrl+↑", "ctrl+←", "ctrl+→"} {
		require.True(t, helpKeyByHelpText(u.ShortHelp(), helpKey), "ShortHelp missing %q", helpKey)
		require.True(t, fullHelpKeyByHelpText(u.FullHelp(), helpKey), "FullHelp missing %q", helpKey)
	}
}

// TestShortFullHelpHidePrevNextWithSingleSibling: viewing a child session
// with only one sibling must still show exit, but not prev/next since
// there's nothing to cycle to.
func TestShortFullHelpHidePrevNextWithSingleSibling(t *testing.T) {
	t.Parallel()

	u := newHelpTestUI(t)
	u.sess.navStack = []sessionNavFrame{
		{
			parentSessionID: "parent-session",
			siblings: []childSessionRef{
				{messageID: "msg1", toolCallID: "tc-1"},
			},
			siblingIndex: 0,
		},
	}

	require.True(t, helpKeyByHelpText(u.ShortHelp(), "ctrl+↑"))
	require.True(t, fullHelpKeyByHelpText(u.FullHelp(), "ctrl+↑"))
	for _, helpKey := range []string{"ctrl+←", "ctrl+→"} {
		require.False(t, helpKeyByHelpText(u.ShortHelp(), helpKey), "ShortHelp must not show %q", helpKey)
		require.False(t, fullHelpKeyByHelpText(u.FullHelp(), helpKey), "FullHelp must not show %q", helpKey)
	}
}

// TestShortFullHelpBaselineNoChildSessionBindings: with an empty nav stack
// and no item selected, none of the four child-session bindings appear.
func TestShortFullHelpBaselineNoChildSessionBindings(t *testing.T) {
	t.Parallel()

	u := newHelpTestUI(t)

	for _, helpKey := range []string{"ctrl+↓", "ctrl+↑", "ctrl+←", "ctrl+→"} {
		require.False(t, helpKeyByHelpText(u.ShortHelp(), helpKey), "ShortHelp must not show %q", helpKey)
		require.False(t, fullHelpKeyByHelpText(u.FullHelp(), helpKey), "FullHelp must not show %q", helpKey)
	}
}
