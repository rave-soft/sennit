package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// newTestQuestionEditor builds a bare questionEditor for exercising
// the shared note machine directly.
func newTestQuestionEditor(t *testing.T) *questionEditor {
	t.Helper()
	s := styles.SennitDark()
	e := newQuestionEditor(&s)
	return &e
}

// TestQuestionEditorOpenNote verifies opening a note focuses the
// editor and pre-populates it with existing text, or resets it when
// there's nothing saved yet.
func TestQuestionEditorOpenNote(t *testing.T) {
	t.Parallel()

	t.Run("fresh note starts empty and focused", func(t *testing.T) {
		t.Parallel()
		e := newTestQuestionEditor(t)
		e.openNote("choice_a")

		require.Equal(t, "choice_a", e.activeNoteKey)
		require.True(t, e.noteEditor.Focused())
		require.Empty(t, e.noteEditor.Value())
	})

	t.Run("reopening pre-populates saved text", func(t *testing.T) {
		t.Parallel()
		e := newTestQuestionEditor(t)
		e.notes["choice_a"] = "existing note"

		e.openNote("choice_a")

		require.Equal(t, "existing note", e.noteEditor.Value())
	})
}

// TestQuestionEditorCloseNote verifies closing the note saves
// trimmed non-empty text and deletes the entry for blank text.
func TestQuestionEditorCloseNote(t *testing.T) {
	t.Parallel()

	t.Run("saves trimmed text", func(t *testing.T) {
		t.Parallel()
		e := newTestQuestionEditor(t)
		e.openNote("choice_a")
		e.noteEditor.SetValue("  a real note  ")

		e.closeNote("choice_a")

		require.Empty(t, e.activeNoteKey)
		require.False(t, e.noteEditor.Focused())
		require.Equal(t, "a real note", e.notes["choice_a"])
	})

	t.Run("blank text deletes any existing note", func(t *testing.T) {
		t.Parallel()
		e := newTestQuestionEditor(t)
		e.notes["choice_a"] = "old note"
		e.openNote("choice_a")
		e.noteEditor.SetValue("   ")

		e.closeNote("choice_a")

		_, ok := e.notes["choice_a"]
		require.False(t, ok, "whitespace-only note text should clear the saved note")
	})
}

// TestQuestionEditorHandleNoteKey exercises every branch of the note
// editor's key handling: close, nav (up/down), enter, and default
// passthrough.
func TestQuestionEditorHandleNoteKey(t *testing.T) {
	t.Parallel()

	t.Run("close key invokes onClose and is handled", func(t *testing.T) {
		t.Parallel()
		e := newTestQuestionEditor(t)
		e.openNote("choice_a")
		e.noteEditor.SetValue("draft")
		closed := false

		cmd, handled := e.handleNoteKey(tea.KeyPressMsg{Code: tea.KeyEscape}, CloseKey, func() { closed = true })

		require.True(t, handled)
		require.Nil(t, cmd)
		require.True(t, closed)
	})

	t.Run("up nav invokes onClose but is not handled, letting the caller navigate", func(t *testing.T) {
		t.Parallel()
		e := newTestQuestionEditor(t)
		e.openNote("choice_a")
		closed := false

		cmd, handled := e.handleNoteKey(tea.KeyPressMsg{Code: tea.KeyUp}, CloseKey, func() { closed = true })

		require.False(t, handled)
		require.Nil(t, cmd)
		require.True(t, closed)
	})

	t.Run("down nav invokes onClose but is not handled", func(t *testing.T) {
		t.Parallel()
		e := newTestQuestionEditor(t)
		e.openNote("choice_a")
		closed := false

		cmd, handled := e.handleNoteKey(tea.KeyPressMsg{Code: tea.KeyDown}, CloseKey, func() { closed = true })

		require.False(t, handled)
		require.Nil(t, cmd)
		require.True(t, closed)
	})

	t.Run("enter invokes onClose and is handled", func(t *testing.T) {
		t.Parallel()
		e := newTestQuestionEditor(t)
		e.openNote("choice_a")
		closed := false

		cmd, handled := e.handleNoteKey(tea.KeyPressMsg{Code: tea.KeyEnter}, CloseKey, func() { closed = true })

		require.True(t, handled)
		require.Nil(t, cmd)
		require.True(t, closed)
	})

	t.Run("default types into the note editor", func(t *testing.T) {
		t.Parallel()
		e := newTestQuestionEditor(t)
		e.openNote("choice_a")

		cmd, handled := e.handleNoteKey(tea.KeyPressMsg{Code: 'x', Text: "x"}, CloseKey, func() { t.Fatal("onClose must not be called") })

		require.True(t, handled)
		require.Nil(t, cmd)
		require.Equal(t, "x", e.noteEditor.Value())
	})
}

// TestQuestionEditorNoteCursor verifies noteCursor returns nil when
// unfocused and an offset hardware cursor when focused.
func TestQuestionEditorNoteCursor(t *testing.T) {
	t.Parallel()

	e := newTestQuestionEditor(t)
	require.Nil(t, e.noteCursor(2, 5, 2), "unfocused note editor has no cursor")

	e.openNote("choice_a")
	cur := e.noteCursor(2, 5, 2)
	require.NotNil(t, cur)
	// areaMinX(5) + 1 + prefixWidth(2) plus the textarea's own
	// column (0 for an empty note), and screenRow(2) added to Y.
	require.Equal(t, 8, cur.X)
	require.Equal(t, 2, cur.Y)
}

// TestQuestionEditorHandlePaste verifies paste routes to whichever
// textarea is focused — the note editor takes priority, then the
// fill-in, and it's a no-op when neither is focused.
func TestQuestionEditorHandlePaste(t *testing.T) {
	t.Parallel()

	t.Run("routes to the note editor when active", func(t *testing.T) {
		t.Parallel()
		e := newTestQuestionEditor(t)
		e.openNote("choice_a")

		cmd := e.handlePaste(tea.PasteMsg{Content: "note paste"})

		require.Nil(t, cmd)
		require.Equal(t, "note paste", e.noteEditor.Value())
	})

	t.Run("routes to the fill-in when it's focused", func(t *testing.T) {
		t.Parallel()
		e := newTestQuestionEditor(t)
		e.fillIn.Focus()

		cmd := e.handlePaste(tea.PasteMsg{Content: "fillin paste"})

		require.Nil(t, cmd)
		require.Equal(t, "fillin paste", e.fillIn.Value())
	})

	t.Run("no-op when nothing is focused", func(t *testing.T) {
		t.Parallel()
		e := newTestQuestionEditor(t)

		cmd := e.handlePaste(tea.PasteMsg{Content: "dropped"})

		require.Nil(t, cmd)
		require.Empty(t, e.noteEditor.Value())
		require.Empty(t, e.fillIn.Value())
	})
}
