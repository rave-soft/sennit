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
