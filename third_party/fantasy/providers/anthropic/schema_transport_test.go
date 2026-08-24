package anthropic

import (
	"encoding/json"
	"testing"

	"charm.land/fantasy"
)

func TestAnthropicToolsPreserveSchemaExtrasAndRequired(t *testing.T) {
	source := map[string]any{"type": "object", "$defs": map[string]any{"id": map[string]any{"type": "string"}}, "properties": map[string]any{"value": map[string]any{"$ref": "#/$defs/id"}}, "required": []any{"value"}, "additionalProperties": false}
	model := languageModel{}
	raw, _, _, _ := model.toTools([]fantasy.Tool{fantasy.FunctionTool{Name: "lookup", InputSchema: source}}, nil, false)
	var wire map[string]any
	if err := json.Unmarshal(raw[0], &wire); err != nil {
		t.Fatal(err)
	}
	input := wire["input_schema"].(map[string]any)
	if input["additionalProperties"] != false {
		t.Fatalf("extras lost: %#v", input)
	}
	if _, ok := input["$defs"]; !ok {
		t.Fatal("$defs lost")
	}
	required := input["required"].([]any)
	if len(required) != 1 || required[0] != "value" {
		t.Fatalf("required lost: %#v", required)
	}
}
