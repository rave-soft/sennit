package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

// TestAtTriggerMidTextUsesCursorPosition covers a regression: the "@"
// completion trigger used to anchor completionsStartIndex at
// len(textarea.Value()) regardless of where the cursor actually was, so
// typing "@" in the middle of existing text pointed the completion splice
// at the end of the buffer instead of where "@" was typed. Moving the
// cursor back into the middle of "hi world" and typing "@" there must
// start the completion right after "hi ", not after the whole buffer.
func TestAtTriggerMidTextUsesCursorPosition(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	typeText(u, "hi world")
	require.Equal(t, "hi world", u.editor.textarea.Value())

	// Move the cursor to just after "hi " (byte offset 3), a whitespace
	// boundary, so the "@" trigger is allowed to fire there.
	u.editor.textarea.SetCursorColumn(3)

	u.handleKeyPressMsg(tea.KeyPressMsg{Text: "@", Code: '@'})

	require.True(t, u.editor.completionsOpen)
	require.Equal(t, "hi @world", u.editor.textarea.Value())
	require.Equal(t, 3, u.editor.completionsStartIndex,
		"completion must start where '@' was typed, not at the end of the buffer")

	// "@world" (no space between "@" and "world") is one contiguous word,
	// so the completion replaces the whole thing, same as it would at the
	// end of a buffer — insertCompletionText also appends a trailing space.
	require.True(t, u.editor.insertCompletionText("there.go"))
	require.Equal(t, "hi there.go ", u.editor.textarea.Value())
}
