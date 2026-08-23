package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestQuestionFormBracketKeyTypedIntoFreeText is the regression test for
// "[" and "]" being intercepted as tab-nav shortcuts before reaching the
// active component: a focused FreeText editor must receive the literal
// character instead of the form switching tabs out from under the user.
func TestQuestionFormBracketKeyTypedIntoFreeText(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	batch := question.Request{
		ID: "batch1",
		Questions: []question.Question{
			{ID: "q1", Type: question.TypeFreeText, Text: "List an array literal"},
			{ID: "q2", Type: question.TypeYesNo, Text: "Sure?"},
		},
		ConfirmTitle: "Confirm",
	}
	f := NewQuestionForm(&s, batch)
	require.Equal(t, 0, f.activeIdx)

	freeText, ok := f.questions[0].(*FreeText)
	require.True(t, ok)
	freeText.editor.Focus()

	done, cmd := f.HandleKey(tea.KeyPressMsg{Code: '[', Text: "["})
	require.False(t, done)
	require.Nil(t, cmd)

	require.Equal(t, 0, f.activeIdx, "the bracket key must not switch tabs while typed into FreeText")
	require.Contains(t, freeText.editor.Value(), "[")
}

// TestQuestionFormBracketKeySwitchesTabsOutsideFreeText confirms the
// bracket shortcuts still work as tab navigation for question types that
// don't accept free text (e.g. YesNo), so the fix doesn't disable the
// shortcut globally.
func TestQuestionFormBracketKeySwitchesTabsOutsideFreeText(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	batch := question.Request{
		ID: "batch1",
		Questions: []question.Question{
			{ID: "q1", Type: question.TypeYesNo, Text: "Sure?"},
			{ID: "q2", Type: question.TypeFreeText, Text: "Why?"},
		},
		ConfirmTitle: "Confirm",
	}
	f := NewQuestionForm(&s, batch)
	require.Equal(t, 0, f.activeIdx)

	done, cmd := f.HandleKey(tea.KeyPressMsg{Code: ']', Text: "]"})
	require.False(t, done)
	require.Nil(t, cmd)

	require.Equal(t, 1, f.activeIdx, "the bracket key must still navigate tabs outside FreeText")
}

// TestQuestionFormEscBlursFocusedFreeTextInsteadOfCancelling is the
// regression test for esc being consumed by the form's global close
// handler before it reaches the focused component: pressing esc while
// typing an answer used to cancel the entire batch, discarding whatever
// the user had typed (here and on any other tab). It should instead just
// blur the field, the same way it stops editing rather than throwing work
// away.
func TestQuestionFormEscBlursFocusedFreeTextInsteadOfCancelling(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	batch := question.Request{
		ID: "batch1",
		Questions: []question.Question{
			{ID: "q1", Type: question.TypeFreeText, Text: "Describe the bug"},
		},
	}

	var cancelled bool
	f := NewQuestionForm(&s, batch)
	f.OnCancel = func() tea.Cmd { cancelled = true; return nil }

	freeText, ok := f.questions[0].(*FreeText)
	require.True(t, ok)
	freeText.editor.Focus()

	for _, r := range "unsaved thoughts" {
		done, _ := f.HandleKey(keyMsg(r))
		require.False(t, done)
	}
	require.Equal(t, "unsaved thoughts", freeText.editor.Value())

	done, cmd := f.HandleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.False(t, done, "esc on a focused field must not submit or cancel the batch")
	require.Nil(t, cmd)
	require.False(t, cancelled, "the typed answer must not be discarded by a blur")
	require.False(t, freeText.editor.Focused(), "esc should blur the field")
	require.Equal(t, "unsaved thoughts", freeText.editor.Value(), "typed text must survive the blur")

	// With the field already blurred, esc now falls through to the usual
	// cancel behavior.
	done, _ = f.HandleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.True(t, done)
	require.True(t, cancelled)
}
