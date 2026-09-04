package tools

import (
	"encoding/json"
	"fmt"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/stretchr/testify/require"
)

// TestQuestionDescriptionMatchesEnforcedLimits is the regression test for
// finding G: question.md used to hard-code limits ("under 300 chars",
// "under 100") that were half the actual enforced values
// (question.MaxDescriptionLength=600, question.MaxChoiceDescriptionLength=200),
// and never mentioned the question-text or choice-label limits at all.
// Reading the numbers from the template's rendered output, rather than
// re-typing them here, keeps this test from drifting the same way the old
// hand-written prose did.
func TestQuestionDescriptionMatchesEnforcedLimits(t *testing.T) {
	desc := questionDescription()
	require.NotEmpty(t, desc)
	require.Contains(t, desc, fmt.Sprintf("under %d chars", question.MaxDescriptionLength))
	require.Contains(t, desc, fmt.Sprintf("under %d chars", question.MaxChoiceLabelLength))
	require.Contains(t, desc, fmt.Sprintf("under %d chars", question.MaxChoiceDescriptionLength))
	require.Contains(t, desc, fmt.Sprintf("under %d chars", question.MaxQuestionLength))
	require.Contains(t, desc, fmt.Sprintf("Max %d choices", question.MaxChoices))
	require.Contains(t, desc, fmt.Sprintf("Max %d questions", question.MaxQuestions))
	require.Contains(t, desc, "unique", "must document that a repeated choice id is an error")
	require.NotContains(t, desc, "under 300 chars", "must not still carry the stale hard-coded limit")
	require.NotContains(t, desc, "under 100 chars", "must not still carry the stale hard-coded limit")
}

func TestQuestionItemGetChoicesPrefersNonEmptyChoicesAlias(t *testing.T) {
	preferred := []QuestionChoice{{ID: "choice", Label: "Choice"}}
	fallback := []QuestionChoice{{ID: "option", Label: "Option"}}

	require.Equal(t, preferred, (QuestionItem{Choices: preferred, Options: fallback}).GetChoices())
	require.Equal(t, fallback, (QuestionItem{Options: fallback}).GetChoices())
}

func TestQuestionParamsUnmarshalJSON_NativeArray(t *testing.T) {
	t.Parallel()
	input := `{"questions": [{"type": "yes_no", "question": "OK?", "description": "test"}]}`
	var p QuestionParams
	require.NoError(t, json.Unmarshal([]byte(input), &p))
	require.Len(t, p.Questions, 1)
	require.Equal(t, "OK?", p.Questions[0].Question)
}

func TestQuestionParamsUnmarshalJSON_StringEncodedArray(t *testing.T) {
	t.Parallel()
	// Simulates a model that double-serializes the questions field.
	inner := `[{"type":"yes_no","question":"OK?","description":"test"}]`
	encoded, _ := json.Marshal(inner)
	input := `{"questions": ` + string(encoded) + `}`
	var p QuestionParams
	require.NoError(t, json.Unmarshal([]byte(input), &p))
	require.Len(t, p.Questions, 1)
	require.Equal(t, "OK?", p.Questions[0].Question)
}

func TestQuestionParamsUnmarshalJSON_StringEncodedWithWhitespace(t *testing.T) {
	t.Parallel()
	inner := `  [{"type":"single_choice","question":"Pick","description":"d","choices":[{"id":"a","label":"A"}]}]  `
	encoded, _ := json.Marshal(inner)
	input := `{"questions": ` + string(encoded) + `, "confirm_title": "Go?"}`
	var p QuestionParams
	require.NoError(t, json.Unmarshal([]byte(input), &p))
	require.Len(t, p.Questions, 1)
	require.Equal(t, "Pick", p.Questions[0].Question)
	require.Equal(t, "Go?", p.ConfirmTitle)
}

func TestQuestionParamsUnmarshalJSON_InvalidString(t *testing.T) {
	t.Parallel()
	encoded, _ := json.Marshal("not valid json")
	input := `{"questions": ` + string(encoded) + `}`
	var p QuestionParams
	require.Error(t, json.Unmarshal([]byte(input), &p))
}

func TestFormatAnswer_MultiChoiceWithFillIn(t *testing.T) {
	answer := question.Answer{
		SelectedIDs: []string{"speed", "readability"},
		FillInText:  "maintainability",
	}
	resp := formatAnswer(&answer, question.TypeMultiChoice)
	require.Contains(t, resp.Content, `User selected: ["speed","readability"]`)
	require.Contains(t, resp.Content, "User provided: maintainability")
}

func TestFormatAnswer_SelectionsOnly(t *testing.T) {
	answer := question.Answer{SelectedIDs: []string{"gardening"}}
	resp := formatAnswer(&answer, question.TypeSingleChoice)
	require.Contains(t, resp.Content, `User selected: ["gardening"]`)
	require.NotContains(t, resp.Content, "User provided")
}

func TestFormatAnswer_Skipped(t *testing.T) {
	answer := question.Answer{}
	resp := formatAnswer(&answer, question.TypeFreeText)
	require.Equal(t, "User skipped this question", resp.Content)
}

// TestQuestionTool_MissingSessionIDIsRejected verifies the question tool
// checks for an empty session ID like its neighbors (e.g. ask_parent,
// task_cancel): a missing session ID is a wiring bug, not something the
// model's call can fix, so it must surface as a Go error rather than
// reaching svc.Ask.
func TestQuestionTool_MissingSessionIDIsRejected(t *testing.T) {
	t.Parallel()

	tool := NewQuestionTool(nil)

	input, err := json.Marshal(QuestionParams{
		Questions: []QuestionItem{
			{Type: "yes_no", Question: "OK?", Description: "test"},
		},
	})
	require.NoError(t, err)

	_, err = tool.Run(t.Context(), fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.Error(t, err)
	require.Contains(t, err.Error(), "session ID is required for question")
}
