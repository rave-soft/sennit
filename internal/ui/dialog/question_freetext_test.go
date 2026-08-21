package dialog

import (
	"image"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// newTestFreeText builds a FreeText question component for tests.
func newTestFreeText(t *testing.T) *FreeText {
	t.Helper()
	s := styles.SennitDark()
	req := question.Question{
		ID:          "q1",
		Type:        question.TypeFreeText,
		Text:        "Tell me a story",
		Description: "Take your time",
	}
	return NewFreeText(&s, req)
}

// typeText focuses the editor and feeds each rune of s to it as a
// key press.
func typeText(t *testing.T, d *FreeText, s string) {
	t.Helper()
	d.editor.Focus()
	for _, r := range s {
		_, cmd := d.HandleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
		require.Nil(t, cmd)
	}
}

// TestFreeTextHandleKeyClose verifies pressing esc closes with an
// empty answer.
func TestFreeTextHandleKeyClose(t *testing.T) {
	t.Parallel()

	d := newTestFreeText(t)
	typeText(t, d, "draft")

	done, cmd := d.HandleKey(tea.KeyPressMsg{Code: tea.KeyEscape})

	require.True(t, done)
	require.Nil(t, cmd)
	require.Equal(t, question.Answer{QuestionID: "q1"}, d.lastResponse)
}

// TestFreeTextHandleKeyEnterSubmitsNonEmpty verifies Enter submits
// the trimmed editor content and reports done.
func TestFreeTextHandleKeyEnterSubmitsNonEmpty(t *testing.T) {
	t.Parallel()

	d := newTestFreeText(t)
	typeText(t, d, "  hello world  ")

	done, cmd := d.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.True(t, done)
	require.Nil(t, cmd)
	require.Equal(t, "hello world", d.lastResponse.FillInText)
	require.Equal(t, "q1", d.lastResponse.QuestionID)
}

// TestFreeTextHandleKeyEnterIgnoresEmpty verifies Enter on a blank
// (or whitespace-only) editor does not submit.
func TestFreeTextHandleKeyEnterIgnoresEmpty(t *testing.T) {
	t.Parallel()

	d := newTestFreeText(t)
	typeText(t, d, "   ")

	done, cmd := d.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.False(t, done)
	require.Nil(t, cmd)
	require.Equal(t, question.Answer{}, d.lastResponse)
}

// TestFreeTextHandleKeyNewline verifies both newline bindings insert
// a line break instead of submitting.
func TestFreeTextHandleKeyNewline(t *testing.T) {
	t.Parallel()

	t.Run("shift+enter", func(t *testing.T) {
		t.Parallel()
		d := newTestFreeText(t)
		typeText(t, d, "line one")
		done, cmd := d.HandleKey(tea.KeyPressMsg{Text: "shift+enter"})
		require.False(t, done)
		require.Nil(t, cmd)
		typeText(t, d, "line two")
		require.Equal(t, "line one\nline two", d.editor.Value())
	})

	t.Run("ctrl+j", func(t *testing.T) {
		t.Parallel()
		d := newTestFreeText(t)
		typeText(t, d, "a")
		done, cmd := d.HandleKey(tea.KeyPressMsg{Text: "ctrl+j"})
		require.False(t, done)
		require.Nil(t, cmd)
		typeText(t, d, "b")
		require.Equal(t, "a\nb", d.editor.Value())
	})
}

// TestFreeTextHandleKeyDefaultPassesToEditor verifies unmatched keys
// fall through to the textarea (the default editor-passthrough arm).
func TestFreeTextHandleKeyDefaultPassesToEditor(t *testing.T) {
	t.Parallel()

	d := newTestFreeText(t)
	d.editor.Focus()
	done, _ := d.HandleKey(tea.KeyPressMsg{Code: 'x', Text: "x"})

	require.False(t, done)
	require.Equal(t, "x", d.editor.Value())
}

// TestFreeTextResponsePreservesUnsavedText verifies Response returns
// live editor content even before it's been submitted via answer(),
// so tabbing away keeps typed text.
func TestFreeTextResponsePreservesUnsavedText(t *testing.T) {
	t.Parallel()

	d := newTestFreeText(t)
	typeText(t, d, "unsaved")

	require.Equal(t, question.Answer{QuestionID: "q1", FillInText: "unsaved"}, d.Response())
}

// TestFreeTextResponseFallsBackToLastResponse verifies Response
// returns the last committed answer once the editor is empty again.
func TestFreeTextResponseFallsBackToLastResponse(t *testing.T) {
	t.Parallel()

	d := newTestFreeText(t)
	d.answer(question.Answer{QuestionID: "q1", FillInText: "committed"})

	require.Equal(t, question.Answer{QuestionID: "q1", FillInText: "committed"}, d.Response())
}

// TestFreeTextGetRequest verifies GetRequest round-trips the request
// the component was constructed with.
func TestFreeTextGetRequest(t *testing.T) {
	t.Parallel()

	d := newTestFreeText(t)
	require.Equal(t, d.Request, d.GetRequest())
}

// TestFreeTextShortHelp verifies the status-bar bindings are the
// enter/newline/close trio.
func TestFreeTextShortHelp(t *testing.T) {
	t.Parallel()

	d := newTestFreeText(t)
	help := d.ShortHelp()
	require.Len(t, help, 3)
	require.Equal(t, d.keyEnter, help[0])
	require.Equal(t, d.keyNewline, help[1])
	require.Equal(t, d.keyClose, help[2])
}

// TestFreeTextHeight verifies Height grows with a description and
// falls back to the last drawn width when passed <= 0.
func TestFreeTextHeight(t *testing.T) {
	t.Parallel()

	d := newTestFreeText(t)
	withDesc := d.Height(60)
	require.Positive(t, withDesc)

	noDescReq := question.Question{ID: "q2", Type: question.TypeFreeText, Text: "short"}
	s := styles.SennitDark()
	nd := NewFreeText(&s, noDescReq)
	withoutDesc := nd.Height(60)
	require.Less(t, withoutDesc, withDesc, "a description should add height")

	// width <= 0 falls back to lastWidth, then to choiceListMaxWidth.
	require.Equal(t, nd.Height(choiceListMaxWidth), nd.Height(0))
}

// TestFreeTextDraw verifies Draw renders content into the screen
// buffer and returns a cursor while focused.
func TestFreeTextDraw(t *testing.T) {
	t.Parallel()

	d := newTestFreeText(t)
	d.SetFocused(true)
	typeText(t, d, "hi")

	scr := uv.NewScreenBuffer(60, 20)
	cur := d.Draw(scr, image.Rect(0, 0, 60, 20))

	require.NotNil(t, cur)
	require.Equal(t, 60, d.lastWidth)
}

// TestFreeTextDrawScrollbarOnOverflow verifies a narrow viewport
// forces the overflow/rebuild path (and scrollbar) without panicking
// and without losing the cursor.
func TestFreeTextDrawScrollbarOnOverflow(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	req := question.Question{
		ID:          "q1",
		Type:        question.TypeFreeText,
		Text:        "A very long question that will definitely need to wrap across several lines to overflow the tiny viewport",
		Description: "And an equally long description to push the content well past a two-line viewport, forcing scroll handling",
	}
	d := NewFreeText(&s, req)
	d.SetFocused(true)

	require.Greater(t, d.Height(20), 3, "content must outgrow the 3-row viewport for this test to exercise overflow")

	scr := uv.NewScreenBuffer(20, 3)
	cur := d.Draw(scr, image.Rect(0, 0, 20, 3))
	if cur != nil {
		require.GreaterOrEqual(t, cur.Y, 0)
		require.Less(t, cur.Y, 3, "cursor must stay within the drawn viewport")
	}
	require.GreaterOrEqual(t, d.scrollOffset, 0)
}

// TestFreeTextSetFocused verifies focus toggles the textarea's
// focus state.
func TestFreeTextSetFocused(t *testing.T) {
	t.Parallel()

	d := newTestFreeText(t)
	d.SetFocused(true)
	require.True(t, d.focused)
	require.True(t, d.editor.Focused())

	d.SetFocused(false)
	require.False(t, d.focused)
	require.False(t, d.editor.Focused())
}

// TestFreeTextHandleWheel verifies wheel scrolling accumulates the
// offset and marks wheel-active mode.
func TestFreeTextHandleWheel(t *testing.T) {
	t.Parallel()

	d := newTestFreeText(t)
	d.HandleWheel(0, 3)
	require.Equal(t, 3, d.scrollOffset)
	require.True(t, d.wheelActive)

	d.HandleWheel(0, 0)
	require.Equal(t, 3, d.scrollOffset, "a zero deltaY should not change the offset")
}

// TestFreeTextHandlePaste verifies paste events reach the editor.
func TestFreeTextHandlePaste(t *testing.T) {
	t.Parallel()

	d := newTestFreeText(t)
	d.editor.Focus()
	cmd := d.HandlePaste(tea.PasteMsg{Content: "pasted text"})
	require.Nil(t, cmd)
	require.Equal(t, "pasted text", d.editor.Value())
}

// TestFreeTextHandleMouseClickNoop verifies the no-op contract.
func TestFreeTextHandleMouseClickNoop(t *testing.T) {
	t.Parallel()

	d := newTestFreeText(t)
	done, handled := d.HandleMouseClick(5, 5)
	require.False(t, done)
	require.False(t, handled)
}

// TestFreeTextHeightChanged verifies the pure-height contract: it
// always reports false since Height() takes no render-time state.
func TestFreeTextHeightChanged(t *testing.T) {
	t.Parallel()

	d := newTestFreeText(t)
	require.False(t, d.HeightChanged())
}

// TestFreeTextSetHoverNoop verifies SetHover is a documented no-op
// (free text has no hoverable elements).
func TestFreeTextSetHoverNoop(t *testing.T) {
	t.Parallel()

	d := newTestFreeText(t)
	d.SetHover(5, 5) // must not panic; nothing to assert
}
