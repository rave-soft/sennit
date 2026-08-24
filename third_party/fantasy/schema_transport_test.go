package fantasy

import (
	"context"
	"reflect"
	"testing"
)

type schemaTransportTool struct{ info ToolInfo }

func (t schemaTransportTool) Info() ToolInfo { return t.info }
func (schemaTransportTool) Run(_ context.Context, _ ToolCall) (ToolResponse, error) {
	return ToolResponse{}, nil
}
func (schemaTransportTool) ProviderOptions() ProviderOptions   { return nil }
func (schemaTransportTool) SetProviderOptions(ProviderOptions) {}

func TestPrepareToolsPreservesCompleteSchemaAndDoesNotMutateSource(t *testing.T) {
	source := map[string]any{"type": "object", "$defs": map[string]any{"x": map[string]any{"type": "string"}}, "properties": map[string]any{"value": map[string]any{"$ref": "#/$defs/x"}}, "required": []any{"value"}, "additionalProperties": false}
	a := &agent{}
	got := a.prepareTools([]AgentTool{schemaTransportTool{ToolInfo{Name: "x", InputSchema: source}}}, nil, nil, false)
	function := got[0].(FunctionTool)
	if !reflect.DeepEqual(source, function.InputSchema) {
		t.Fatalf("schema changed: %#v", function.InputSchema)
	}
	function.InputSchema["additionalProperties"] = true
	if source["additionalProperties"] != false {
		t.Fatal("prepared schema aliases source")
	}
}

func TestPrepareToolsSupportsLegacyToolInfo(t *testing.T) {
	a := &agent{}
	got := a.prepareTools([]AgentTool{schemaTransportTool{ToolInfo{Name: "x", Parameters: map[string]any{"id": map[string]any{"type": "string"}}, Required: []string{"id"}}}}, nil, nil, false)
	schema := got[0].(FunctionTool).InputSchema
	if schema["type"] != "object" || !reflect.DeepEqual(schema["required"], []string{"id"}) {
		t.Fatalf("legacy schema = %#v", schema)
	}
}
