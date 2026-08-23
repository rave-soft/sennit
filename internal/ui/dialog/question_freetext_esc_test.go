package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// TestFreeTextEscSurvivesARedraw pins the two-esc contract against the
// thing that used to defeat it. The first esc blurs the editor so the
// second can cancel the batch — but the form is redrawn every frame and
// the draw calls SetFocused(focus == editor), which re-focused the editor
// straight away. The second esc therefore found it focused and blurred it
// "again", so esc never cancelled a free-text question in a running app.
// The test that covered this passed only because it never drew between
// the two presses.
func TestFreeTextEscSurvivesARedraw(t *testing.T) {
	t.Parallel()

	sty := styles.Theme("")
	ft := NewFreeText(&sty, question.Question{ID: "q1", Text: "why?"})
	ft.SetFocused(true)
	require.True(t, ft.editor.Focused())

	ft.BlurForEsc()
	require.True(t, ft.BlurredByEsc())
	require.False(t, ft.editor.Focused())

	// A frame goes by: this is the call every Draw makes.
	ft.SetFocused(true)

	require.True(t, ft.BlurredByEsc(), "a redraw must not undo the esc")
	require.False(t, ft.editor.Focused(), "a redraw must not take the keyboard back")
}

// TestFreeTextTypingAfterEscTakesTheKeyboardBack keeps the blur from
// becoming a trap: esc steps out of the editor, and anything else steps
// back in.
func TestFreeTextTypingAfterEscTakesTheKeyboardBack(t *testing.T) {
	t.Parallel()

	sty := styles.Theme("")
	ft := NewFreeText(&sty, question.Question{ID: "q1", Text: "why?"})
	ft.SetFocused(true)
	ft.BlurForEsc()

	done, _ := ft.HandleKey(tea.KeyPressMsg{Code: 'a', Text: "a"})
	require.False(t, done)
	require.False(t, ft.BlurredByEsc())
	require.True(t, ft.editor.Focused())
	require.Equal(t, "a", ft.editor.Value())
}

// TestQuestionFormEscCancelsOnTheSecondPressAcrossRedraws is the same
// invariant through the form, with a draw-time SetFocused between the two
// presses — the sequence a person actually performs.
func TestQuestionFormEscCancelsOnTheSecondPressAcrossRedraws(t *testing.T) {
	t.Parallel()

	sty := styles.Theme("")
	form := NewQuestionForm(&sty, question.Request{
		ID: "r1",
		Questions: []question.Question{
			{ID: "q1", Text: "why?", Type: question.TypeFreeText},
		},
	})
	form.SetFocused(true)

	done, _ := form.HandleKey(escKey)
	require.False(t, done, "the first esc leaves the editor, it does not cancel")

	// The frame in between.
	form.SetFocused(true)

	done, _ = form.HandleKey(escKey)
	require.True(t, done, "the second esc must cancel the batch")
}
