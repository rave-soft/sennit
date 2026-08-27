package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

// TestCheckBangModeAfterPasteMultiByteWhitespace covers a regression: the
// cursor fixup after stripping the bang prefix computed
// `col - (len(val) - len(stripped))`, mixing a byte length (len on the
// raw strings) with col, a rune column. A multi-byte whitespace character
// ahead of "!" (here EM SPACE, U+2003, 3 bytes in UTF-8) then throws the
// cursor off by the difference between its byte and rune width.
//
// Pasting EM-SPACE + "!echo" onto an empty editor strips the leading EM
// SPACE and the "!", leaving "echo" with the cursor landing at the end
// (column 4) right after SetValue. The fixup then shifts it left by the
// removed prefix's width: 2 runes (correct) vs. 4 bytes (buggy), landing
// on column 2 vs. column 0.
func TestCheckBangModeAfterPasteMultiByteWhitespace(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	u.focus = uiFocusEditor

	emSpace := string(rune(0x2003))

	// The pasted text has no ASCII space, so fsext.ParsePastedFiles treats
	// it as a single file-path candidate: handlePasteMsg detours through
	// the async pasteFilesCheckedMsg stat-check. Resolving that message
	// directly (rather than through the full Update loop) avoids needing
	// a workspace stub for the unrelated staleness-refresh commands that
	// batch onto a full Update.
	cmd := u.handlePasteMsg(tea.PasteMsg{Content: emSpace + "!echo"})
	require.NotNil(t, cmd)
	msg, ok := cmd().(pasteFilesCheckedMsg)
	require.True(t, ok, "expected the async stat-check message, got %T", msg)
	u.applyPasteFilesChecked(msg)

	require.True(t, u.editor.bangMode)
	require.Equal(t, "echo", u.editor.textarea.Value())
	require.Equal(t, 2, u.editor.textarea.Column(),
		"cursor must be shifted left by the removed prefix's rune count, not its byte count")
}
