package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

// TestPageKeysScrollChatWhileEditorFocused is the regression test for
// PgUp/PgDn doing nothing: the editor holds focus almost all the time (only
// child-session drill-in moves focus to the chat), so the page keys have to
// scroll the conversation from there rather than falling through to the
// textarea.
func TestPageKeysScrollChatWhileEditorFocused(t *testing.T) {
	t.Parallel()

	u := scrollbarOverflowUI(t)
	require.Equal(t, uiFocusEditor, u.focus, "the editor must hold focus for this test to mean anything")

	u.chat.ScrollToBottom()
	bottom := u.chat.Offset()
	require.Positive(t, bottom, "content must overflow for paging to be observable")

	_, _ = u.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyPgUp}))
	afterPageUp := u.chat.Offset()
	require.Less(t, afterPageUp, bottom, "pgup must scroll the conversation up")
	require.False(t, u.chat.Follow(), "scrolling up must leave follow mode")

	_, _ = u.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyPgDown}))
	require.Greater(t, u.chat.Offset(), afterPageUp, "pgdown must scroll back down")
}

// TestPageKeysDoNotTypeIntoEditor pins the other half of the fix: the page
// keys are consumed by the chat scroll, never inserted into the prompt.
func TestPageKeysDoNotTypeIntoEditor(t *testing.T) {
	t.Parallel()

	u := scrollbarOverflowUI(t)
	u.editor.textarea.SetValue("draft")

	_, _ = u.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyPgUp}))
	_, _ = u.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyPgDown}))

	require.Equal(t, "draft", u.editor.textarea.Value())
}
