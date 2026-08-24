package openai

import (
	"reflect"
	"testing"

	"charm.land/fantasy"
)

func fullSchema() map[string]any {
	return map[string]any{
		"type": "object", "$defs": map[string]any{"id": map[string]any{"type": "string"}},
		"properties": map[string]any{"value": map[string]any{"$ref": "#/$defs/id"}},
		"required":   []any{"value"}, "additionalProperties": false,
	}
}

func TestOpenAIChatToolsPreserveFullSchema(t *testing.T) {
	source := fullSchema()
	got, _, _ := toOpenAiTools([]fantasy.Tool{fantasy.FunctionTool{Name: "lookup", InputSchema: source}}, nil)
	schema := map[string]any(got[0].OfFunction.Function.Parameters)
	if !reflect.DeepEqual(source, schema) {
		t.Fatalf("schema lost: %#v", schema)
	}
	schema["additionalProperties"] = true
	if source["additionalProperties"] != false {
		t.Fatal("final wire schema aliases source")
	}
}

func TestOpenAIResponsesToolsPreserveFullSchema(t *testing.T) {
	source := fullSchema()
	got, _, _ := toResponsesTools([]fantasy.Tool{fantasy.FunctionTool{Name: "lookup", InputSchema: source}}, nil, nil)
	schema := got[0].OfFunction.Parameters
	if !reflect.DeepEqual(source, schema) {
		t.Fatalf("schema lost: %#v", schema)
	}
	schema["additionalProperties"] = true
	if source["additionalProperties"] != false {
		t.Fatal("final wire schema aliases source")
	}
}
