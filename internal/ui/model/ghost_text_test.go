package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

// TestGhostTextPrefixMatch_MostRecentWins covers the core prediction scan:
// history index 0 is the most recent (see historyPrev), so when multiple
// entries share the typed prefix, the one at the lowest index wins.
func TestGhostTextPrefixMatch_MostRecentWins(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.editor.promptHistory.messages = []string{
		"git commit -m fix",   // most recent, index 0
		"git commit -m older", // index 1, also matches but shouldn't win
	}
	u.editor.textarea.SetValue("git commit")

	require.Equal(t, " -m fix", u.activeGhostTail())
}

// TestGhostTextNoMatch covers the non-matching cases: empty input, no
// history entry with the typed prefix, and an entry that's a prefix of the
// input rather than the other way around (not strictly longer).
func TestGhostTextNoMatch(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.editor.promptHistory.messages = []string{"hello world"}

	u.editor.textarea.SetValue("")
	require.Empty(t, u.activeGhostTail(), "empty input must not predict")

	u.editor.textarea.SetValue("goodbye")
	require.Empty(t, u.activeGhostTail(), "non-matching prefix must not predict")

	u.editor.textarea.SetValue("hello world")
	require.Empty(t, u.activeGhostTail(), "input equal to the history entry must not predict")
}

// TestGhostTextTabAcceptsFullMultilineTail verifies Tab inserts the whole
// tail — every line, not just the first line shown in the overlay — and
// leaves the cursor at the end.
func TestGhostTextTabAcceptsFullMultilineTail(t *testing.T) {
	t.Parallel()

	u := newEscTestUI(t)
	u.editor.promptHistory.messages = []string{"line one\nline two\nline three"}
	u.editor.textarea.SetValue("line one")

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyTab})

	require.Equal(t, "line one\nline two\nline three", u.editor.textarea.Value())
	require.Empty(t, u.activeGhostTail(), "no prediction should remain active after acceptance")
}

// TestGhostTextTabNoopWithoutPrediction ensures Tab never inserts a literal
// tab character when there's nothing to accept.
func TestGhostTextTabNoopWithoutPrediction(t *testing.T) {
	t.Parallel()

	u := newEscTestUI(t)
	u.editor.textarea.SetValue("no history matches this")

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyTab})

	require.Equal(t, "no history matches this", u.editor.textarea.Value(), "Tab must be a no-op, not insert a literal tab")
}

// TestGhostTextEscHidesPrediction verifies a first (non-consecutive) Esc
// hides the currently shown prediction for the same textarea value, without
// interfering with the existing double-Esc-to-clear-draft behavior.
func TestGhostTextEscHidesPrediction(t *testing.T) {
	t.Parallel()

	u := newEscTestUI(t)
	u.editor.promptHistory.messages = []string{"hello world"}
	u.editor.textarea.SetValue("hello")
	u.editor.promptHistory.index = -1

	require.Equal(t, " world", u.activeGhostTail(), "sanity check: prediction active before Esc")

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Empty(t, u.activeGhostTail(), "first Esc must hide the active prediction")

	// Typing more (changing the value away from the one Esc hid) implicitly
	// un-hides it; the hide only applies to that exact hidden value.
	u.editor.textarea.SetValue("hello w")
	require.Equal(t, "orld", u.activeGhostTail(), "changing the value un-hides the prediction")
}

// TestGhostTextSuppressedByCompletionsOrBangMode ensures the prediction
// never shows while the completions popup is open or the editor is in bang
// (!) shell mode, even when the input would otherwise prefix-match history.
func TestGhostTextSuppressedByCompletionsOrBangMode(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.editor.promptHistory.messages = []string{"hello world"}
	u.editor.textarea.SetValue("hello")

	u.editor.completions.open = true
	require.Empty(t, u.activeGhostTail(), "must not predict while completions popup is open")
	u.editor.completions.open = false

	u.editor.bang.enter(false)
	require.Empty(t, u.activeGhostTail(), "must not predict in bang mode")
	u.editor.bang.exit()

	require.Equal(t, " world", u.activeGhostTail(), "sanity check: prediction active with both flags cleared")
}
