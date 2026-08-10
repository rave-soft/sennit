package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/braid/internal/session"
	"github.com/stretchr/testify/require"
)

// TestBangTypedAtEmptyEditorEntersBangMode covers the entry path: typing
// "!" as the first character strips it from the visible text and flips
// bangMode, mirroring Claude Code's "!" shell prefix.
func TestBangTypedAtEmptyEditorEntersBangMode(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	u.handleKeyPressMsg(tea.KeyPressMsg{Text: "!", Code: '!'})

	require.True(t, u.editor.bangMode)
	require.True(t, u.editor.bangWasEmpty)
	require.Empty(t, u.editor.textarea.Value(), "the leading '!' is stripped, not shown")

	typeText(u, "echo hi")
	require.Equal(t, "echo hi", u.editor.textarea.Value())
	require.False(t, u.editor.bangWasEmpty)
}

// TestBangBackspaceOnEmptyExits covers the exit path symmetric to entry:
// backspacing the last character of an in-progress bang command drops back
// to the normal prompt.
func TestBangBackspaceOnEmptyExits(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	typeText(u, "!")
	require.True(t, u.editor.bangMode)

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyBackspace})
	require.False(t, u.editor.bangMode)
	require.Empty(t, u.editor.textarea.Value())
}

// TestBangEnterClearsBangModeAndDispatchesShell covers the run path: Enter
// on a non-empty bang command exits bang mode and clears the editor before
// handing off to runShellCommand (whose own workspace-facing behavior is
// exercised elsewhere; this only asserts the editor-side contract).
func TestBangEnterClearsBangModeAndDispatchesShell(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	u.session = &session.Session{ID: "sess-1"} // has a session already; skip CreateSession
	typeText(u, "!echo hi")
	require.True(t, u.editor.bangMode)

	cmd := u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.False(t, u.editor.bangMode, "Enter exits bang mode")
	require.Empty(t, u.editor.textarea.Value(), "the editor is cleared")
	require.NotNil(t, cmd, "Enter must dispatch the shell command")
}

// TestBangSuppressesSlashCompletions and TestBangSuppressesAtCompletions
// guard against the "/" and "@" completion triggers firing while the user
// is typing a shell command in bang mode — e.g. "!/usr/bin/env" or
// "!git log @{u}" are plain shell text, not completion triggers. Before
// this was guarded, typing "/" at the very start of a bang command (the
// textarea is empty right after "!" strips itself) would open the command
// popup and hijack the next Enter into running a slash command instead of
// the shell command.
func TestBangSuppressesSlashCompletions(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	typeText(u, "!/usr/bin/env")

	require.True(t, u.editor.bangMode)
	require.False(t, u.editor.completionsOpen, "'/' must not open the command popup in bang mode")
	require.Equal(t, "/usr/bin/env", u.editor.textarea.Value())
}

func TestBangSuppressesAtCompletions(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	typeText(u, "!git log @{u}")

	require.True(t, u.editor.bangMode)
	require.False(t, u.editor.completionsOpen, "'@' must not open the file-mention popup in bang mode")
	require.Equal(t, "git log @{u}", u.editor.textarea.Value())
}

// TestSlashAndAtCompletionsUnaffectedOutsideBangMode is the control case:
// the suppression above must not leak into ordinary (non-bang) editing.
func TestSlashAndAtCompletionsUnaffectedOutsideBangMode(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	u.handleKeyPressMsg(tea.KeyPressMsg{Text: "/", Code: '/'})
	require.True(t, u.editor.completionsOpen)
	require.Equal(t, completionsModeCommand, u.editor.completionsMode)
}

// TestBangGhostSuggestionRestoredThroughHistory verifies "!"-prefixed
// entries built by loadPromptHistory (see history.go) round-trip through Up
// navigation: the stored "!cmd" form re-enters bang mode and strips the
// prefix again, matching TestHistoryBangCommandStripsPrefixWhileAlreadyInBangMode
// but starting from a non-bang editor.
func TestBangHistoryUpEntersBangModeFromPlainEditor(t *testing.T) {
	t.Parallel()

	u := newEscTestUI(t)
	u.editor.promptHistory.messages = []string{"!echo one", "a plain message"}
	u.editor.promptHistory.index = -1

	require.True(t, u.historyPrev())
	require.True(t, u.editor.bangMode)
	require.Equal(t, "echo one", u.editor.textarea.Value())

	require.True(t, u.historyPrev())
	require.False(t, u.editor.bangMode, "the older entry is a plain message, not a bang command")
	require.Equal(t, "a plain message", u.editor.textarea.Value())
}

// TestEscapeAfterHistoryNavRestoresBangDraft covers a regression: the
// prompt-history draft is captured from the textarea's raw Value(), which
// never carries the "!" once bang mode has stripped it. Without restoring
// that prefix (see editorState.draftValue), browsing away from an
// in-progress bang command with Up and then back with Esc silently dropped
// out of bang mode even though the command text itself was preserved.
func TestEscapeAfterHistoryNavRestoresBangDraft(t *testing.T) {
	t.Parallel()

	u := newEscTestUI(t)
	u.editor.promptHistory.messages = []string{"older message"}
	u.editor.promptHistory.index = -1
	u.editor.bangMode = true
	u.editor.textarea.InsertString("ls -la")

	require.True(t, u.historyPrev())
	require.Equal(t, "older message", u.editor.textarea.Value())
	require.False(t, u.editor.bangMode, "the history entry itself has no bang prefix")

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, "ls -la", u.editor.textarea.Value(), "first Esc restores the draft")
	require.True(t, u.editor.bangMode, "bang mode must be restored along with the draft")
}

// TestBangModeDraftSurvivesHistoryNavigation is the end-to-end version of
// the above, driven through the real key-routing path (typing "!ls -la",
// Up, Esc) rather than calling historyPrev/handleHistoryEscape directly.
func TestBangModeDraftSurvivesHistoryNavigation(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	u.editor.promptHistory.messages = []string{"previous prompt"}

	typeText(u, "!ls -la")
	require.True(t, u.editor.bangMode)
	require.Equal(t, "ls -la", u.editor.textarea.Value())

	// Move to the start so Up enters history navigation instead of normal
	// cursor movement (see handleHistoryUp).
	u.editor.textarea.CursorStart()
	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, "previous prompt", u.editor.textarea.Value())
	require.False(t, u.editor.bangMode)

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, "ls -la", u.editor.textarea.Value(), "Esc restores the in-progress bang draft")
	require.True(t, u.editor.bangMode, "bang mode must be restored along with the draft")
}
