package dialog

import (
	"image"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// newTestYesNo builds a YesNo question component for tests.
func newTestYesNo(t *testing.T) *YesNo {
	t.Helper()
	s := styles.SennitDark()
	req := question.Question{
		ID:          "q1",
		Type:        question.TypeYesNo,
		Text:        "Proceed?",
		Description: "This cannot be undone",
	}
	return NewYesNo(&s, req)
}

var escKey = tea.KeyPressMsg{Code: tea.KeyEscape}

// ---- SingleChoice.HandleKey ----

// TestSingleChoiceHandleKeyClose verifies esc dismisses with an
// empty answer.
func TestSingleChoiceHandleKeyClose(t *testing.T) {
	t.Parallel()

	d := newTestSingleChoice(t)
	done, cmd := d.HandleKey(escKey)

	require.True(t, done)
	require.Nil(t, cmd)
	require.Equal(t, question.Answer{QuestionID: "q1"}, d.lastResponse)
}

// TestSingleChoiceHandleKeyEnterSelects verifies Enter submits the
// choice under the cursor.
func TestSingleChoiceHandleKeyEnterSelects(t *testing.T) {
	t.Parallel()

	d := newTestSingleChoice(t)
	d.cursorIdx = 1

	done, cmd := d.HandleKey(enterKey)

	require.True(t, done)
	require.Nil(t, cmd)
	require.Equal(t, []string{"b"}, d.lastResponse.SelectedIDs)
}

// TestSingleChoiceHandleKeyNumberSelects verifies a digit key jumps
// the cursor to that choice without submitting.
func TestSingleChoiceHandleKeyNumberSelects(t *testing.T) {
	t.Parallel()

	d := newTestSingleChoice(t)
	done, cmd := d.HandleKey(tea.KeyPressMsg{Code: '2', Text: "2"})

	require.False(t, done)
	require.Nil(t, cmd)
	require.Equal(t, 1, d.cursorIdx, "'2' selects the second (zero-based index 1) choice")
}

// TestSingleChoiceHandleKeyNav verifies up/down move the cursor and
// are reported as consumed.
func TestSingleChoiceHandleKeyNav(t *testing.T) {
	t.Parallel()

	d := newTestSingleChoice(t)
	d.cursorIdx = 0

	done, cmd := d.HandleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	require.False(t, done)
	require.Nil(t, cmd)
	require.Equal(t, 1, d.cursorIdx)

	done, cmd = d.HandleKey(tea.KeyPressMsg{Code: tea.KeyUp})
	require.False(t, done)
	require.Nil(t, cmd)
	require.Equal(t, 0, d.cursorIdx)
}

// TestSingleChoiceHandleKeyNote verifies alt+n opens the note
// editor for the current cursor item.
func TestSingleChoiceHandleKeyNote(t *testing.T) {
	t.Parallel()

	d := newTestSingleChoice(t)
	d.cursorIdx = 0

	done, cmd := d.HandleKey(tea.KeyPressMsg{Text: "alt+n"})

	require.False(t, done)
	require.Nil(t, cmd)
	require.Equal(t, "a", d.activeNoteKey)
	require.True(t, d.noteEditor.Focused())
}

// TestSingleChoiceHandleKeyFillInFocusEntersThenSubmits exercises
// the fill-in mode entry (Enter while on the fill-in item) and the
// submit path once inside it.
func TestSingleChoiceHandleKeyFillInFocusEntersThenSubmits(t *testing.T) {
	t.Parallel()

	d := newTestSingleChoice(t)
	d.cursorIdx = len(d.Request.Choices) // land on the fill-in row

	// Navigate onto the fill-in row: handleNavKey auto-focuses it,
	// the real keyboard entry point into fill-in mode.
	d.cursorIdx = len(d.Request.Choices) - 1
	done, cmd := d.HandleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	require.False(t, done)
	require.Nil(t, cmd)
	require.True(t, d.isFillIn())
	require.True(t, d.fillIn.Focused())

	// Type some text into the fill-in.
	d.fillIn.SetValue("custom")

	// Enter again submits, going through handleFillInFocused's onDone.
	done, cmd = d.HandleKey(enterKey)
	require.True(t, done)
	require.Nil(t, cmd)
	require.Equal(t, "custom", d.lastResponse.FillInText)
}

// TestSingleChoiceHandleKeyFillInEmptyEnterDoesNotSubmit verifies
// that submitting an empty fill-in doesn't close the dialog.
func TestSingleChoiceHandleKeyFillInEmptyEnterDoesNotSubmit(t *testing.T) {
	t.Parallel()

	d := newTestSingleChoice(t)
	d.cursorIdx = len(d.Request.Choices)
	d.fillIn.Focus()

	done, cmd := d.HandleKey(enterKey)

	require.False(t, done)
	require.Nil(t, cmd)
}

// TestSingleChoiceHandleKeyFillInCloseBlurs verifies esc while the
// fill-in is focused blurs it and answers the choice cursor,
// without closing the whole dialog.
func TestSingleChoiceHandleKeyFillInCloseBlurs(t *testing.T) {
	t.Parallel()

	d := newTestSingleChoice(t)
	d.cursorIdx = len(d.Request.Choices)
	d.fillIn.Focus()

	done, cmd := d.HandleKey(escKey)

	require.True(t, done, "esc from the fill-in commits the current answer per onClose")
	require.Nil(t, cmd)
	require.False(t, d.fillIn.Focused())
}

// TestSingleChoiceHandleKeyDefaultTypesIntoFillIn verifies an
// unmatched key while the fill-in is focused types into it (the
// default editor-passthrough arm of handleFillInKey).
func TestSingleChoiceHandleKeyDefaultTypesIntoFillIn(t *testing.T) {
	t.Parallel()

	d := newTestSingleChoice(t)
	d.cursorIdx = len(d.Request.Choices)
	d.fillIn.Focus()

	done, _ := d.HandleKey(tea.KeyPressMsg{Code: 'z', Text: "z"})

	require.False(t, done)
	require.Equal(t, "z", d.fillIn.Value())
}

// TestSingleChoiceHandleKeyNoteEditorTakesPriority verifies that
// while the note editor is focused, its own key handling runs
// before the rest of HandleKey (e.g. arrow keys don't move the
// choice cursor).
func TestSingleChoiceHandleKeyNoteEditorTakesPriority(t *testing.T) {
	t.Parallel()

	d := newTestSingleChoice(t)
	d.cursorIdx = 0
	d.openNote(d.noteKey())
	d.noteEditor.SetValue("wip")

	done, cmd := d.HandleKey(tea.KeyPressMsg{Code: 'y', Text: "y"})

	require.False(t, done)
	require.Nil(t, cmd)
	require.Equal(t, "wipy", d.noteEditor.Value())
}

// TestSingleChoiceGetRequest verifies the round-trip accessor.
func TestSingleChoiceGetRequest(t *testing.T) {
	t.Parallel()

	d := newTestSingleChoice(t)
	require.Equal(t, d.Request, d.GetRequest())
}

// TestSingleChoiceHandleMouseClick verifies a click on a choice row
// selects it, a click on the fill-in row focuses it, and a miss is
// reported as not handled.
func TestSingleChoiceHandleMouseClick(t *testing.T) {
	t.Parallel()

	// Each subtest gets its own component: HandleMouseClick mutates
	// shared state (cursor, selection), so subtests can't safely
	// run in parallel against one instance.
	newDrawn := func(t *testing.T) *SingleChoice {
		t.Helper()
		d := newTestSingleChoice(t)
		scr := uv.NewScreenBuffer(40, 20)
		d.Draw(scr, image.Rect(0, 0, 40, 20))
		return d
	}

	t.Run("miss when no compositor hit", func(t *testing.T) {
		t.Parallel()
		d := newDrawn(t)
		done, handled := d.HandleMouseClick(-1, -1)
		require.False(t, done)
		require.False(t, handled)
	})

	t.Run("hit on a choice selects it", func(t *testing.T) {
		t.Parallel()
		d := newDrawn(t)
		hit := d.choiceCompositor.Hit(0, 2)
		require.False(t, hit.Empty(), "test setup expects row 2 to hit a choice")
		done, handled := d.HandleMouseClick(0, 2)
		require.False(t, done)
		require.True(t, handled)
	})
}

// TestSingleChoiceDraw verifies Draw renders without panicking and
// records the drawn width.
func TestSingleChoiceDraw(t *testing.T) {
	t.Parallel()

	d := newTestSingleChoice(t)
	scr := uv.NewScreenBuffer(40, 20)
	d.Draw(scr, image.Rect(0, 0, 40, 20))
	require.Equal(t, 40, d.lastWidth)
}

// ---- MultiChoice.HandleKey ----

// TestMultiChoiceHandleKeyClose verifies esc dismisses with an
// empty answer.
func TestMultiChoiceHandleKeyClose(t *testing.T) {
	t.Parallel()

	d := newTestMultiChoice(t)
	done, cmd := d.HandleKey(escKey)

	require.True(t, done)
	require.Nil(t, cmd)
	require.Equal(t, question.Answer{QuestionID: "q1"}, d.lastResponse)
}

// TestMultiChoiceHandleKeyDoneSubmits verifies Enter (the "done"
// key) submits the current selection.
func TestMultiChoiceHandleKeyDoneSubmits(t *testing.T) {
	t.Parallel()

	d := newTestMultiChoice(t)
	d.selected[0] = true

	done, cmd := d.HandleKey(enterKey)

	require.True(t, done)
	require.Nil(t, cmd)
	require.Equal(t, []string{"a"}, d.lastResponse.SelectedIDs)
}

// TestMultiChoiceHandleKeyToggle verifies space toggles the choice
// under the cursor on, then off again.
func TestMultiChoiceHandleKeyToggle(t *testing.T) {
	t.Parallel()

	d := newTestMultiChoice(t)
	d.cursorIdx = 0

	done, cmd := d.HandleKey(spaceKey)
	require.False(t, done)
	require.Nil(t, cmd)
	require.True(t, d.selected[0])

	done, cmd = d.HandleKey(spaceKey)
	require.False(t, done)
	require.Nil(t, cmd)
	_, stillSet := d.selected[0]
	require.False(t, stillSet, "toggling again should remove the selection entirely")
}

// TestMultiChoiceHandleKeyNumberTogglesAndMoves verifies a digit key
// both moves the cursor and toggles that choice, and that pressing
// the same digit again untoggles it — deleting the map entry rather
// than merely flipping it to false — so a re-press doesn't leave a
// stale selection in Response().
func TestMultiChoiceHandleKeyNumberTogglesAndMoves(t *testing.T) {
	t.Parallel()

	d := newTestMultiChoice(t)
	done, cmd := d.HandleKey(tea.KeyPressMsg{Code: '3', Text: "3"})

	require.False(t, done)
	require.Nil(t, cmd)
	require.Equal(t, 2, d.cursorIdx)
	require.True(t, d.selected[2])

	done, cmd = d.HandleKey(tea.KeyPressMsg{Code: '3', Text: "3"})
	require.False(t, done)
	require.Nil(t, cmd)
	_, stillSet := d.selected[2]
	require.False(t, stillSet, "pressing the same number key again should remove the selection entirely")
	require.NotContains(t, d.Response().SelectedIDs, "c",
		"an untoggled choice must not survive into the response")
}

// TestMultiChoiceHandleKeyNav verifies up/down navigate.
func TestMultiChoiceHandleKeyNav(t *testing.T) {
	t.Parallel()

	d := newTestMultiChoice(t)
	d.cursorIdx = 0

	done, cmd := d.HandleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	require.False(t, done)
	require.Nil(t, cmd)
	require.Equal(t, 1, d.cursorIdx)
}

// TestMultiChoiceHandleKeyNote verifies alt+n opens a note.
func TestMultiChoiceHandleKeyNote(t *testing.T) {
	t.Parallel()

	d := newTestMultiChoice(t)
	d.cursorIdx = 1

	done, cmd := d.HandleKey(tea.KeyPressMsg{Text: "alt+n"})

	require.False(t, done)
	require.Nil(t, cmd)
	require.Equal(t, "b", d.activeNoteKey)
}

// TestMultiChoiceHandleKeyFillInFocusEntersThenSubmits exercises the
// fill-in entry (toggle key focuses it) and submit path.
func TestMultiChoiceHandleKeyFillInFocusEntersThenSubmits(t *testing.T) {
	t.Parallel()

	d := newTestMultiChoice(t)
	d.cursorIdx = len(d.Request.Choices)

	done, cmd := d.HandleKey(spaceKey)
	require.False(t, done)
	require.Nil(t, cmd)
	require.True(t, d.fillIn.Focused())

	d.fillIn.SetValue("other")

	done, cmd = d.HandleKey(enterKey)
	require.True(t, done)
	require.Nil(t, cmd)
	require.Equal(t, "other", d.lastResponse.FillInText)
}

// TestMultiChoiceHandleKeyFillInEmptyEnterDoesNotSubmit verifies an
// empty fill-in doesn't close the dialog via done key.
func TestMultiChoiceHandleKeyFillInEmptyEnterDoesNotSubmit(t *testing.T) {
	t.Parallel()

	d := newTestMultiChoice(t)
	d.cursorIdx = len(d.Request.Choices)
	d.fillIn.Focus()

	done, cmd := d.HandleKey(enterKey)

	require.False(t, done)
	require.Nil(t, cmd)
}

// TestMultiChoiceHandleKeyFillInCloseBlurs verifies esc from the
// fill-in blurs it without submitting.
func TestMultiChoiceHandleKeyFillInCloseBlurs(t *testing.T) {
	t.Parallel()

	d := newTestMultiChoice(t)
	d.cursorIdx = len(d.Request.Choices)
	d.fillIn.Focus()

	done, cmd := d.HandleKey(escKey)

	require.False(t, done)
	require.Nil(t, cmd)
	require.False(t, d.fillIn.Focused())
}

// TestMultiChoiceHandleKeyDefaultTypesIntoFillIn verifies typing
// while the fill-in is focused reaches the textarea.
func TestMultiChoiceHandleKeyDefaultTypesIntoFillIn(t *testing.T) {
	t.Parallel()

	d := newTestMultiChoice(t)
	d.cursorIdx = len(d.Request.Choices)
	d.fillIn.Focus()

	done, _ := d.HandleKey(tea.KeyPressMsg{Code: 'q', Text: "q"})

	require.False(t, done)
	require.Equal(t, "q", d.fillIn.Value())
}

// TestMultiChoiceHandleKeyNoteEditorTakesPriority verifies keys
// route to the note editor while it's focused.
func TestMultiChoiceHandleKeyNoteEditorTakesPriority(t *testing.T) {
	t.Parallel()

	d := newTestMultiChoice(t)
	d.cursorIdx = 0
	d.openNote(d.noteKey())

	done, cmd := d.HandleKey(tea.KeyPressMsg{Code: 'n', Text: "n"})

	require.False(t, done)
	require.Nil(t, cmd)
	require.Equal(t, "n", d.noteEditor.Value())
}

// TestMultiChoiceGetRequest verifies the round-trip accessor.
func TestMultiChoiceGetRequest(t *testing.T) {
	t.Parallel()

	d := newTestMultiChoice(t)
	require.Equal(t, d.Request, d.GetRequest())
}

// TestMultiChoiceHandleMouseClick verifies clicking a choice toggles
// it on, and clicking it again untoggles it — deleting the map entry
// (not just flipping it to false) so a re-click doesn't leave a stale
// selection in Response().
func TestMultiChoiceHandleMouseClick(t *testing.T) {
	t.Parallel()

	d := newTestMultiChoice(t)
	scr := uv.NewScreenBuffer(40, 20)
	d.Draw(scr, image.Rect(0, 0, 40, 20))

	done, handled := d.HandleMouseClick(0, 2)
	require.False(t, done)
	require.True(t, handled)
	require.True(t, d.selected[0], "clicking a choice row should toggle it on")

	done, handled = d.HandleMouseClick(0, 2)
	require.False(t, done)
	require.True(t, handled)
	_, stillSet := d.selected[0]
	require.False(t, stillSet, "clicking the same choice again should remove the selection entirely")
	require.NotContains(t, d.Response().SelectedIDs, "a",
		"an untoggled choice must not survive into the response")
}

// TestMultiChoiceDraw verifies Draw renders without panicking.
func TestMultiChoiceDraw(t *testing.T) {
	t.Parallel()

	d := newTestMultiChoice(t)
	scr := uv.NewScreenBuffer(40, 20)
	d.Draw(scr, image.Rect(0, 0, 40, 20))
	require.Equal(t, 40, d.lastWidth)
}

// TestMultiChoiceHeightAndSetters exercise the small delegate
// methods that just forward to choiceList.
func TestMultiChoiceHeightAndSetters(t *testing.T) {
	t.Parallel()

	d := newTestMultiChoice(t)
	require.Positive(t, d.Height(40))
	require.False(t, d.HeightChanged())

	d.SetFocused(true)
	require.True(t, d.focused)

	d.SetHover(1, 1)
	require.True(t, d.mouseActive)

	cmd := d.HandlePaste(tea.PasteMsg{Content: "x"})
	_ = cmd // no textarea focused; verifying no panic and nil-safe routing
}

// ---- YesNo.HandleKey ----

// TestYesNoHandleKeyClose verifies esc dismisses with an empty
// answer.
func TestYesNoHandleKeyClose(t *testing.T) {
	t.Parallel()

	d := newTestYesNo(t)
	done, cmd := d.HandleKey(escKey)

	require.True(t, done)
	require.Nil(t, cmd)
	require.Equal(t, question.Answer{QuestionID: "q1"}, d.lastResponse)
}

// TestYesNoHandleKeyLeftRightToggles verifies left/right (and h/l)
// flip the selection.
func TestYesNoHandleKeyLeftRightToggles(t *testing.T) {
	t.Parallel()

	d := newTestYesNo(t)
	require.True(t, d.selectedNo, "defaults to No for safety")

	done, cmd := d.HandleKey(tea.KeyPressMsg{Code: tea.KeyLeft})
	require.False(t, done)
	require.Nil(t, cmd)
	require.False(t, d.selectedNo)

	done, cmd = d.HandleKey(tea.KeyPressMsg{Code: tea.KeyRight})
	require.False(t, done)
	require.Nil(t, cmd)
	require.True(t, d.selectedNo)
}

// TestYesNoHandleKeyEnterConfirmsSelection verifies Enter submits
// whatever is currently selected.
func TestYesNoHandleKeyEnterConfirmsSelection(t *testing.T) {
	t.Parallel()

	d := newTestYesNo(t)
	d.selectedNo = false // Yes selected

	done, cmd := d.HandleKey(enterKey)

	require.True(t, done)
	require.Nil(t, cmd)
	require.NotNil(t, d.lastResponse.Yes)
	require.True(t, *d.lastResponse.Yes)
}

// TestYesNoHandleKeyYN verifies the y/n shortcuts both set the
// selection and submit immediately.
func TestYesNoHandleKeyYN(t *testing.T) {
	t.Parallel()

	t.Run("y answers yes", func(t *testing.T) {
		t.Parallel()
		d := newTestYesNo(t)
		done, cmd := d.HandleKey(tea.KeyPressMsg{Code: 'y', Text: "y"})
		require.True(t, done)
		require.Nil(t, cmd)
		require.False(t, d.selectedNo)
		require.True(t, *d.lastResponse.Yes)
	})

	t.Run("n answers no", func(t *testing.T) {
		t.Parallel()
		d := newTestYesNo(t)
		d.selectedNo = false
		done, cmd := d.HandleKey(tea.KeyPressMsg{Code: 'n', Text: "n"})
		require.True(t, done)
		require.Nil(t, cmd)
		require.True(t, d.selectedNo)
		require.False(t, *d.lastResponse.Yes)
	})
}

// TestYesNoHandleKeyNoteOpensEditor verifies alt+n opens the
// question-level note editor.
func TestYesNoHandleKeyNoteOpensEditor(t *testing.T) {
	t.Parallel()

	d := newTestYesNo(t)
	done, cmd := d.HandleKey(tea.KeyPressMsg{Text: "alt+n"})

	require.False(t, done)
	require.Nil(t, cmd)
	require.Equal(t, "_question", d.activeNoteKey)
	require.True(t, d.noteEditor.Focused())
}

// TestYesNoHandleKeyNoteEditorTakesPriority verifies that while the
// note editor is open, other bindings (like "y") type into the note
// instead of answering the question.
func TestYesNoHandleKeyNoteEditorTakesPriority(t *testing.T) {
	t.Parallel()

	d := newTestYesNo(t)
	d.openNote("_question")

	done, cmd := d.HandleKey(tea.KeyPressMsg{Code: 'y', Text: "y"})

	require.False(t, done)
	require.Nil(t, cmd)
	require.Equal(t, "y", d.noteEditor.Value())
	require.Empty(t, d.lastResponse.QuestionID, "the note editor should absorb the key, not answer the question")
}

// TestYesNoHandleKeyNoteEditorCloseKey verifies esc while the note
// editor is open closes the note rather than the whole dialog.
func TestYesNoHandleKeyNoteEditorCloseKey(t *testing.T) {
	t.Parallel()

	d := newTestYesNo(t)
	d.openNote("_question")
	d.noteEditor.SetValue("a saved note")

	done, cmd := d.HandleKey(escKey)

	require.False(t, done)
	require.Nil(t, cmd)
	require.Empty(t, d.activeNoteKey)
	require.Equal(t, "a saved note", d.notes["_question"])
}

// TestYesNoGetRequest verifies the round-trip accessor.
func TestYesNoGetRequest(t *testing.T) {
	t.Parallel()

	d := newTestYesNo(t)
	require.Equal(t, d.Request, d.GetRequest())
}

// TestYesNoResponse verifies Response mirrors the current selection.
func TestYesNoResponse(t *testing.T) {
	t.Parallel()

	d := newTestYesNo(t)
	require.False(t, *d.Response().Yes, "default selection is No")

	d.selectedNo = false
	require.True(t, *d.Response().Yes)
}

// TestYesNoShortHelp verifies the help bindings differ while the
// note editor is focused vs. not.
func TestYesNoShortHelp(t *testing.T) {
	t.Parallel()

	d := newTestYesNo(t)
	require.Len(t, d.ShortHelp(), 5)

	d.openNote("_question")
	require.Len(t, d.ShortHelp(), 2)
}

// TestYesNoHeight verifies Height grows to include the note editor
// and saved notes, and falls back to lastWidth/choiceListMaxWidth
// when passed <= 0.
func TestYesNoHeight(t *testing.T) {
	t.Parallel()

	d := newTestYesNo(t)
	base := d.Height(60)
	require.Positive(t, base)

	d.notes["_question"] = "a note"
	withSavedNote := d.Height(60)
	require.Greater(t, withSavedNote, base)

	delete(d.notes, "_question")
	d.openNote("_question")
	withOpenEditor := d.Height(60)
	require.Greater(t, withOpenEditor, base)

	fresh := newTestYesNo(t)
	fresh.lastWidth = choiceListMaxWidth
	require.Equal(t, fresh.Height(choiceListMaxWidth), fresh.Height(0),
		"width<=0 must fall back to lastWidth")
}

// TestYesNoDrawAndMouseClick verifies Draw renders the Yes/No
// buttons and that clicking each one answers accordingly.
func TestYesNoDrawAndMouseClick(t *testing.T) {
	t.Parallel()

	d := newTestYesNo(t)
	scr := uv.NewScreenBuffer(40, 10)
	d.Draw(scr, image.Rect(0, 0, 40, 10))
	require.Equal(t, 40, d.lastWidth)
	require.NotNil(t, d.compositor)

	done, handled := d.HandleMouseClick(-100, -100)
	require.False(t, done)
	require.False(t, handled)
}

// TestYesNoSetHover verifies hover coordinates are stored for
// button highlighting.
func TestYesNoSetHover(t *testing.T) {
	t.Parallel()

	d := newTestYesNo(t)
	d.SetHover(3, 4)
	require.Equal(t, 3, d.hoverX)
	require.Equal(t, 4, d.hoverY)
}

// TestYesNoHandlePaste verifies paste routes through the shared
// questionEditor plumbing to the note editor when it's open.
func TestYesNoHandlePaste(t *testing.T) {
	t.Parallel()

	d := newTestYesNo(t)
	d.openNote("_question")

	cmd := d.HandlePaste(tea.PasteMsg{Content: "pasted note"})

	require.Nil(t, cmd)
	require.Equal(t, "pasted note", d.noteEditor.Value())
}

// TestSingleChoiceShortHelp verifies the help bindings switch based
// on note/fill-in focus state.
func TestSingleChoiceShortHelp(t *testing.T) {
	t.Parallel()

	d := newTestSingleChoice(t)
	require.Len(t, d.ShortHelp(), 6)

	d.cursorIdx = len(d.Request.Choices)
	d.fillIn.Focus()
	require.Len(t, d.ShortHelp(), 3)

	d.fillIn.Blur()
	d.openNote(d.noteKey())
	require.Len(t, d.ShortHelp(), 2)
}

// TestSingleChoiceHeightAndSetters exercises the small delegate
// methods that just forward to choiceList.
func TestSingleChoiceHeightAndSetters(t *testing.T) {
	t.Parallel()

	d := newTestSingleChoice(t)
	require.Positive(t, d.Height(40))
	require.False(t, d.HeightChanged())

	d.SetFocused(true)
	require.True(t, d.focused)

	d.SetHover(2, 2)
	require.True(t, d.mouseActive)

	d.fillIn.Focus()
	cmd := d.HandlePaste(tea.PasteMsg{Content: "pasted"})
	require.Nil(t, cmd)
	require.Equal(t, "pasted", d.fillIn.Value())
}

// TestNumKeyBinding verifies the display-only binding clamps its
// label to the 1-9 range regardless of choice count.
func TestNumKeyBinding(t *testing.T) {
	t.Parallel()

	require.Equal(t, "1-1", numKeyBinding(0).Help().Key)
	require.Equal(t, "1-3", numKeyBinding(3).Help().Key)
	require.Equal(t, "1-9", numKeyBinding(20).Help().Key)
}

// TestMultiChoiceShortHelp verifies the help bindings switch based
// on note/fill-in focus state.
func TestMultiChoiceShortHelp(t *testing.T) {
	t.Parallel()

	d := newTestMultiChoice(t)
	require.Len(t, d.ShortHelp(), 7)

	d.cursorIdx = len(d.Request.Choices)
	d.fillIn.Focus()
	require.Len(t, d.ShortHelp(), 3)

	d.fillIn.Blur()
	d.openNote(d.noteKey())
	require.Len(t, d.ShortHelp(), 2)
}

// TestMultiChoiceHandleMouseClickFillIn verifies clicking the
// fill-in row focuses it without toggling any choice.
func TestMultiChoiceHandleMouseClickFillIn(t *testing.T) {
	t.Parallel()

	d := newTestMultiChoice(t)
	scr := uv.NewScreenBuffer(40, 20)
	d.Draw(scr, image.Rect(0, 0, 40, 20))
	require.GreaterOrEqual(t, d.fillInTop, 0)

	fillY := d.fillInTop
	hit := d.choiceCompositor.Hit(0, fillY)
	require.False(t, hit.Empty(), "test setup expects the fill-in row to be hittable")

	done, handled := d.HandleMouseClick(0, fillY)

	require.False(t, done)
	require.True(t, handled)
	require.True(t, d.fillIn.Focused())
}

// TestYesNoHandleMouseClickButtons verifies clicking each button
// answers accordingly. The button row's exact X offsets are an
// implementation detail, so scan the drawn area for the compositor
// hits rather than hardcoding coordinates.
func TestYesNoHandleMouseClickButtons(t *testing.T) {
	t.Parallel()

	d := newTestYesNo(t)
	scr := uv.NewScreenBuffer(40, 10)
	d.Draw(scr, image.Rect(0, 0, 40, 10))
	require.NotNil(t, d.compositor)

	yesX, yesY, noX, noY := -1, -1, -1, -1
	for y := range 10 {
		for x := range 40 {
			switch common.HitButtonIndex(d.compositor, x, y) {
			case 0:
				if yesX < 0 {
					yesX, yesY = x, y
				}
			case 1:
				if noX < 0 {
					noX, noY = x, y
				}
			}
		}
	}
	require.GreaterOrEqual(t, yesX, 0, "expected to find the Yes button in the drawn area")
	require.GreaterOrEqual(t, noX, 0, "expected to find the No button in the drawn area")

	done, handled := d.HandleMouseClick(yesX, yesY)
	require.True(t, done)
	require.True(t, handled)
	require.True(t, *d.lastResponse.Yes)

	done, handled = d.HandleMouseClick(noX, noY)
	require.True(t, done)
	require.True(t, handled)
	require.False(t, *d.lastResponse.Yes)
}
