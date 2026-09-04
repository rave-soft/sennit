package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"maps"
	"strings"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/question"
)

const QuestionToolName = "question"

//go:embed question.md.tpl
var questionDescriptionTmpl []byte

var questionDescriptionTpl = template.Must(
	template.New("questionDescription").
		Parse(string(questionDescriptionTmpl)),
)

// questionDescriptionData mirrors the limits question.Validate actually
// enforces, so the description can never drift from them the way a
// hand-copied number in prose eventually does.
type questionDescriptionData struct {
	MaxQuestions               int
	MaxChoices                 int
	MaxQuestionLength          int
	MaxDescriptionLength       int
	MaxChoiceLabelLength       int
	MaxChoiceDescriptionLength int
}

func questionDescription() string {
	return renderTemplate(questionDescriptionTpl, questionDescriptionData{
		MaxQuestions:               question.MaxQuestions,
		MaxChoices:                 question.MaxChoices,
		MaxQuestionLength:          question.MaxQuestionLength,
		MaxDescriptionLength:       question.MaxDescriptionLength,
		MaxChoiceLabelLength:       question.MaxChoiceLabelLength,
		MaxChoiceDescriptionLength: question.MaxChoiceDescriptionLength,
	})
}

// QuestionParams defines the parameters for the question tool.
type QuestionParams struct {
	Questions          []QuestionItem `json:"questions" description:"List of questions to present. Single item = no tabs, multiple = tabbed form."`
	ConfirmTitle       string         `json:"confirm_title,omitempty" description:"Title for the confirmation tab shown for multi-question batches. Optional: defaults to a generic title if omitted, but a specific one is worth setting."`
	ConfirmDescription string         `json:"confirm_description,omitempty" description:"Description for the confirmation tab shown for multi-question batches. Optional: defaults to a generic description if omitted, but summarizing the expected answers is worth setting."`
}

// UnmarshalJSON handles models that double-serialize the questions field as a
// JSON string instead of a native array.
func (p *QuestionParams) UnmarshalJSON(data []byte) error {
	type Alias QuestionParams
	aux := &struct {
		Questions json.RawMessage `json:"questions"`
		*Alias
	}{
		Alias: (*Alias)(p),
	}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if len(aux.Questions) == 0 {
		return nil
	}
	// Try array first.
	if err := json.Unmarshal(aux.Questions, &p.Questions); err != nil {
		// Fall back to string-encoded JSON array.
		var s string
		if err2 := json.Unmarshal(aux.Questions, &s); err2 != nil {
			return err
		}
		if err2 := json.Unmarshal([]byte(strings.TrimSpace(s)), &p.Questions); err2 != nil {
			return fmt.Errorf("questions must be an array: %w", err2)
		}
	}
	return nil
}

// QuestionItem is a single question from the tool input.
type QuestionItem struct {
	Label       string           `json:"label,omitempty" description:"Short tab header label (3 words max)."`
	Type        string           `json:"type" description:"The type of question: yes_no, single_choice, multi_choice, or free_text"`
	Question    string           `json:"question" description:"The question text"`
	Description string           `json:"description" description:"Required markdown description shown below the question"`
	Choices     []QuestionChoice `json:"choices,omitempty" description:"List of choices"`
	Options     []QuestionChoice `json:"options,omitempty"` // alias for Choices
}

// GetChoices returns choices, preferring the Choices field over Options.
func (q QuestionItem) GetChoices() []QuestionChoice {
	if len(q.Choices) > 0 {
		return q.Choices
	}
	return q.Options
}

// QuestionChoice represents a selectable option.
type QuestionChoice struct {
	ID          string `json:"id" description:"Unique identifier for this choice"`
	Label       string `json:"label" description:"Display text for this choice"`
	Description string `json:"description,omitempty" description:"Optional description for this choice"`
}

// NewQuestionTool creates a new question tool.
func NewQuestionTool(svc question.Service) fantasy.AgentTool {
	tool := withToolParameterSchema(fantasy.NewAgentTool(
		QuestionToolName,
		questionDescription(),
		func(ctx context.Context, params QuestionParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, missingSessionID(QuestionToolName)
			}

			if len(params.Questions) == 0 {
				return fantasy.NewTextErrorResponse("at least one question is required"), nil
			}
			if len(params.Questions) > question.MaxQuestions {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("exceeds maximum of %d questions per batch (got %d). Split into multiple batches and tell the user there will be follow-up questions", question.MaxQuestions, len(params.Questions))), nil
			}

			questions := make([]question.Question, len(params.Questions))
			for i, item := range params.Questions {
				qType := question.Type(item.Type)
				label := item.Label
				if label == "" {
					label = item.Question
				}
				if qType != question.TypeYesNo && qType != question.TypeSingleChoice && qType != question.TypeMultiChoice && qType != question.TypeFreeText {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("question %d [%s]: invalid type %q (must be yes_no, single_choice, multi_choice, or free_text)", i+1, label, item.Type)), nil
				}
				if item.Question == "" || item.Description == "" {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("question %d [%s]: question and description are required", i+1, label)), nil
				}
				choices := item.GetChoices()
				if (qType == question.TypeSingleChoice || qType == question.TypeMultiChoice) && len(choices) == 0 {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("question %d [%s]: choices are required for %s", i+1, label, qType)), nil
				}
				if qType == question.TypeYesNo && len(choices) != 0 {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("question %d [%s]: choices are not allowed for yes_no", i+1, label)), nil
				}
				for _, choice := range choices {
					if choice.ID == "" || choice.Label == "" {
						return fantasy.NewTextErrorResponse(fmt.Sprintf("question %d [%s]: each choice requires id and label", i+1, label)), nil
					}
				}
				questions[i] = question.Question{
					Type:        qType,
					Label:       item.Label,
					Text:        item.Question,
					Description: item.Description,
					Choices:     convertChoices(item.GetChoices()),
				}
			}

			req := question.Request{
				SessionID:          sessionID,
				ToolCallID:         call.ID,
				Questions:          questions,
				ConfirmTitle:       params.ConfirmTitle,
				ConfirmDescription: params.ConfirmDescription,
			}

			answers, err := svc.Ask(ctx, req)
			if err != nil {
				if errors.Is(err, question.ErrCancelled) {
					resp := fantasy.NewTextErrorResponse("User cancelled this question")
					resp.StopTurn = true
					return resp, nil
				}
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			return formatAnswers(answers, questions)
		},
	), map[string]toolParameterSchema{"questions": {minItems: intPtr(1), maxItems: intPtr(question.MaxQuestions)}, "questions.items.type": {enum: []string{"yes_no", "single_choice", "multi_choice", "free_text"}}, "questions.items.question": {minLength: intPtr(1)}, "questions.items.description": {minLength: intPtr(1)}})
	info := tool.Info()
	questions := info.Parameters["questions"].(map[string]any)
	items := questions["items"].(map[string]any)
	properties := items["properties"].(map[string]any)
	choiceSchemas := make(map[string]map[string]any, 2)
	for _, name := range []string{"choices", "options"} {
		array := properties[name].(map[string]any)
		item := array["items"].(map[string]any)
		itemProperties := item["properties"].(map[string]any)
		selectedProperties := make(map[string]any, len(itemProperties))
		for key, value := range itemProperties {
			property := maps.Clone(value.(map[string]any))
			if key == "id" || key == "label" {
				property["minLength"] = 1
			}
			selectedProperties[key] = property
		}
		selectedItem := maps.Clone(item)
		selectedItem["properties"] = selectedProperties
		choiceSchemas[name] = map[string]any{"type": "array", "minItems": 1, "items": selectedItem}
		array["items"] = map[string]any{}
	}
	choicesPresent := map[string]any{"required": []string{"choices"}, "properties": map[string]any{"choices": map[string]any{"type": "array", "minItems": 1}}}
	optionsPresent := map[string]any{"required": []string{"options"}, "properties": map[string]any{"options": map[string]any{"type": "array", "minItems": 1}}}
	selectedChoices := map[string]any{"required": []string{"choices"}, "properties": map[string]any{"choices": choiceSchemas["choices"]}}
	selectedOptions := map[string]any{"not": choicesPresent, "required": []string{"options"}, "properties": map[string]any{"options": choiceSchemas["options"]}}
	noSelectedChoices := map[string]any{"allOf": []any{map[string]any{"not": choicesPresent}, map[string]any{"not": optionsPresent}}}
	items["allOf"] = []any{
		map[string]any{
			"if":   map[string]any{"properties": map[string]any{"type": map[string]any{"enum": []any{"single_choice", "multi_choice"}}}},
			"then": map[string]any{"anyOf": []any{selectedChoices, selectedOptions}},
		},
		map[string]any{
			"if":   map[string]any{"properties": map[string]any{"type": map[string]any{"const": "free_text"}}},
			"then": map[string]any{"anyOf": []any{selectedChoices, selectedOptions, noSelectedChoices}},
		},
		map[string]any{
			"if": map[string]any{"properties": map[string]any{"type": map[string]any{"const": "yes_no"}}},
			"then": map[string]any{"properties": map[string]any{
				"choices": map[string]any{"type": "array", "maxItems": 0},
				"options": map[string]any{"type": "array", "maxItems": 0},
			}},
		},
	}
	return toolInfoOverride{AgentTool: tool, info: info}
}

func convertChoices(in []QuestionChoice) []question.Choice {
	out := make([]question.Choice, len(in))
	for i, c := range in {
		out[i] = question.Choice{ID: c.ID, Label: c.Label, Description: c.Description}
	}
	return out
}

// formatAnswers converts answers into a tool response string for the LLM.
func formatAnswers(answers []question.Answer, questions []question.Question) (fantasy.ToolResponse, error) {
	var b strings.Builder
	for i, answer := range answers {
		if i > 0 {
			b.WriteString("\n\n")
		}
		if i < len(questions) {
			fmt.Fprintf(&b, "Q%d: %s\n", i+1, questions[i].Text)
		}
		formatted := formatAnswer(&answer, question.Type(""))
		b.WriteString(formatted.Content)
	}
	return fantasy.NewTextResponse(b.String()), nil
}

// formatAnswer formats a single answer for the LLM.
func formatAnswer(answer *question.Answer, _ question.Type) fantasy.ToolResponse {
	var b strings.Builder

	switch {
	case answer.Yes != nil:
		if *answer.Yes {
			b.WriteString("User answered: yes")
		} else {
			b.WriteString("User answered: no")
		}
	case len(answer.SelectedIDs) > 0 || answer.FillInText != "":
		// Selections and free-text can coexist for multi-choice;
		// report both so the model sees the complete answer.
		var parts []string
		if len(answer.SelectedIDs) > 0 {
			data, _ := json.Marshal(answer.SelectedIDs)
			parts = append(parts, fmt.Sprintf("User selected: %s", string(data)))
		}
		if answer.FillInText != "" {
			parts = append(parts, fmt.Sprintf("User provided: %s", answer.FillInText))
		}
		b.WriteString(strings.Join(parts, "\n"))
	default:
		b.WriteString("User skipped this question")
	}

	if len(answer.Notes) > 0 {
		b.WriteString("\n\nNotes:")
		for key, note := range answer.Notes {
			if key == "_question" {
				fmt.Fprintf(&b, "\n- %s", note)
			} else {
				fmt.Fprintf(&b, "\n- [%s]: %s", key, note)
			}
		}
	}

	return fantasy.NewTextResponse(b.String())
}
