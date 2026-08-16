package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/attachments"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

type historyWorkspace struct {
	workspace.Workspace
}

func (historyWorkspace) Config() *config.Config {
	return &config.Config{}
}

func (historyWorkspace) PermissionSkipRequests() bool {
	return false
}

func (historyWorkspace) SupportsThreads() bool {
	return false
}

func TestHistoryBangCommandStripsPrefixWhileAlreadyInBangMode(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.com.Workspace = historyWorkspace{}
	u.editor.promptHistory.messages = []string{"!echo one", "!echo two"}
	u.editor.promptHistory.index = -1

	require.True(t, u.historyPrev())
	require.True(t, u.editor.bangMode)
	require.Equal(t, "echo one", u.editor.textarea.Value())

	require.True(t, u.historyPrev())
	require.True(t, u.editor.bangMode)
	require.Equal(t, "echo two", u.editor.textarea.Value())
}

// newEscTestUI builds a UI in uiChat/uiFocusEditor with the minimal wiring
// handleKeyPressMsg needs (keyMap, dialog overlay, attachments) to drive
// Escape through the real key-routing path rather than calling
// handleHistoryEscape directly.
func newEscTestUI(t *testing.T) *UI {
	t.Helper()
	u := newTestUI()
	u.com.Workspace = historyWorkspace{}
	u.keyMap = DefaultKeyMap()
	u.dialog = dialog.NewOverlay()
	u.editor.attachments = attachments.New(nil, attachments.Keymap{})
	return u
}

// TestDoubleEscClearsTypedDraft covers the "esc esc clears" invariant for
// plain typing (no history navigation involved): the first Esc has
// nothing to exit, so it leaves the draft untouched; the second
// consecutive Esc wipes it.
func TestDoubleEscClearsTypedDraft(t *testing.T) {
	t.Parallel()

	u := newEscTestUI(t)
	u.editor.textarea.InsertString("hello world")
	u.editor.promptHistory.index = -1

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, "hello world", u.editor.textarea.Value(), "first Esc must not clear the draft")

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Empty(t, u.editor.textarea.Value(), "second consecutive Esc must clear the draft")
}

// TestDoubleEscAfterHistoryNavClearsDraft covers the other half: when the
// first Esc has something to do (exit history navigation back to the
// draft), that's all it does — the second consecutive Esc is the one that
// clears the (now-restored) draft.
func TestDoubleEscAfterHistoryNavClearsDraft(t *testing.T) {
	t.Parallel()

	u := newEscTestUI(t)
	u.editor.textarea.InsertString("draft text")
	u.editor.promptHistory.messages = []string{"older message"}
	u.editor.promptHistory.index = -1

	require.True(t, u.historyPrev())
	require.Equal(t, "older message", u.editor.textarea.Value())

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, "draft text", u.editor.textarea.Value(), "first Esc must restore the draft")
	require.Equal(t, -1, u.editor.promptHistory.index)

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Empty(t, u.editor.textarea.Value(), "second consecutive Esc must clear the restored draft")
}

// TestEscThenTypingBreaksDoubleEscSequence: any key other than Escape
// between two Esc presses must reset the "last key was Esc" tracking, so
// a stray typed character doesn't turn a later, unrelated Esc into a
// clearing second-Esc.
func TestEscThenTypingBreaksDoubleEscSequence(t *testing.T) {
	t.Parallel()

	u := newEscTestUI(t)
	u.editor.textarea.InsertString("hello")
	u.editor.promptHistory.index = -1

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.True(t, u.editor.lastKeyWasEsc)

	u.handleKeyPressMsg(tea.KeyPressMsg{Text: "!"})
	require.False(t, u.editor.lastKeyWasEsc, "typing must break the Esc-Esc sequence")

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotEmpty(t, u.editor.textarea.Value(), "this Esc is the first of a new sequence, not a clearing second Esc")
}
