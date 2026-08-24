package google

import (
	"reflect"
	"testing"

	"charm.land/fantasy"
)

func TestGoogleToolsPreserveCompleteJSONSchema(t *testing.T) {
	source := map[string]any{"type": "object", "$defs": map[string]any{"id": map[string]any{"type": "string"}}, "properties": map[string]any{"value": map[string]any{"$ref": "#/$defs/id"}}, "required": []any{"value"}, "additionalProperties": false}
	got, _, _ := toGoogleTools([]fantasy.Tool{fantasy.FunctionTool{Name: "lookup", InputSchema: source}}, nil)
	if !reflect.DeepEqual(source, got[0].ParametersJsonSchema) {
		t.Fatalf("schema lost: %#v", got[0].ParametersJsonSchema)
	}
	gotSchema := got[0].ParametersJsonSchema.(map[string]any)
	gotSchema["additionalProperties"] = true
	if source["additionalProperties"] != false {
		t.Fatal("provider schema aliases source")
	}
}
