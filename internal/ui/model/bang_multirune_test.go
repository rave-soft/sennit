package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

// TestBangEntryMultiRuneKeystrokeMultiByteWhitespace covers the keypress.go
// twin of TestCheckBangModeAfterPasteMultiByteWhitespace: the cursor fixup
// on bang-mode entry computed `col - (len(newVal) - len(stripped))`, a
// byte-length diff applied to col, a rune column. A multi-byte leading
// whitespace rune throws it off.
//
// A single multi-rune keystroke (as an IME composition or a
// programmatically injected KeyPressMsg can deliver) is used here rather
// than typing "!" alone: typing "!" by itself always lands the cursor
// exactly at the boundary being subtracted, so the old and new formulas
// coincidentally agree after SetCursorColumn's 0-clamp. Inserting "!xyz"
// as one keystroke moves the cursor past that boundary, where the
// byte/rune mismatch actually surfaces.
func TestBangEntryMultiRuneKeystrokeMultiByteWhitespace(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)

	emSpace := string(rune(0x2003))
	typeText(u, emSpace)
	require.False(t, u.editor.bangMode)

	u.handleKeyPressMsg(tea.KeyPressMsg{Text: "!xyz"})

	require.True(t, u.editor.bangMode)
	require.Equal(t, "xyz", u.editor.textarea.Value())
	require.Equal(t, 3, u.editor.textarea.Column(),
		"cursor must be shifted left by the removed prefix's rune count, not its byte count")
}
