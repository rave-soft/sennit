package question

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func choiceQuestion(text string) Question {
	return Question{
		ID:          "q1",
		Type:        TypeSingleChoice,
		Text:        text,
		Description: "why this is being asked",
		Choices: []Choice{
			{ID: "a", Label: "Alpha"},
			{ID: "b", Label: "Beta"},
		},
	}
}

// TestValidate_QuestionLengthCountsCharactersNotBytes is the reported
// defect: a question in Cyrillic is about two bytes per character, so a
// 240-character question measured 400-odd bytes and was refused with
// "text exceeds 240 characters" — a message that was not true of the text
// it rejected.
func TestValidate_QuestionLengthCountsCharactersNotBytes(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("я", MaxQuestionLength)
	require.Greater(t, len(text), MaxQuestionLength, "the fixture must be longer in bytes than in characters")
	require.NoError(t, choiceQuestion(text).Validate())

	require.ErrorContains(t, choiceQuestion(text+"я").Validate(), "text exceeds")
}

// TestValidate_QuestionLengthAllows500 pins the raised ceiling: 240 was
// too short for a question that has to name what it is choosing between.
func TestValidate_QuestionLengthAllows500(t *testing.T) {
	t.Parallel()

	require.Equal(t, 500, MaxQuestionLength)
	require.NoError(t, choiceQuestion(strings.Repeat("a", 500)).Validate())
	require.ErrorContains(t, choiceQuestion(strings.Repeat("a", 501)).Validate(), "got 501")
}

// TestValidate_EveryLimitCountsCharacters: the question text was the one
// that showed up in a report, but the description and the two choice
// fields were measured the same way and are just as wrong in bytes.
func TestValidate_EveryLimitCountsCharacters(t *testing.T) {
	t.Parallel()

	q := choiceQuestion("Какой вариант выбрать?")
	q.Description = strings.Repeat("я", MaxDescriptionLength)
	require.NoError(t, q.Validate())

	q.Description = "why"
	q.Choices[0].Label = strings.Repeat("я", MaxChoiceLabelLength)
	require.NoError(t, q.Validate())

	q.Choices[0].Description = strings.Repeat("я", MaxChoiceDescriptionLength)
	require.NoError(t, q.Validate())

	q.Choices[0].Description += "я"
	require.ErrorContains(t, q.Validate(), "description exceeds")
}

// TestValidate_IdentifierCutsOnRuneBoundary: the error's own label
// excerpts the question text, and slicing that by bytes ended a Cyrillic
// label mid-character.
func TestValidate_IdentifierCutsOnRuneBoundary(t *testing.T) {
	t.Parallel()

	q := choiceQuestion(strings.Repeat("я", MaxQuestionLength+1))
	err := q.Validate()
	require.Error(t, err)
	require.True(t, strings.ContainsRune(err.Error(), '…'))
	require.NotContains(t, err.Error(), "�", "the excerpt must not end mid-character")
}
