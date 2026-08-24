package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCloneToolSchemaPreservesFullLocalSchema(t *testing.T) {
	input := map[string]any{
		"type": "object", "description": "root", "default": map[string]any{"kind": "a"},
		"$defs":                map[string]any{"item": map[string]any{"type": "string"}},
		"properties":           map[string]any{"value": map[string]any{"oneOf": []any{map[string]any{"$ref": "#/$defs/item"}, map[string]any{"type": "number"}}}},
		"additionalProperties": false,
	}
	got, err := CloneToolSchema(input)
	if err != nil {
		t.Fatal(err)
	}
	got["description"] = "changed"
	if input["description"] != "root" {
		t.Fatal("clone aliases source schema")
	}
	if _, ok := got["$defs"]; !ok {
		t.Fatal("missing $defs")
	}
	if got["additionalProperties"] != false {
		t.Fatal("missing root keyword")
	}
}

func TestCloneToolSchemaPreservesReferenceLikeAnnotationData(t *testing.T) {
	input := map[string]any{
		"type": "object",
		"default": map[string]any{
			"nested": map[string]any{"$ref": "default.json"},
		},
		"examples": []any{
			map[string]any{"nested": map[string]any{"$ref": "example.json"}},
		},
		"x-vendor": map[string]any{
			"payload": map[string]any{"$ref": "vendor.json"},
		},
	}
	got, err := CloneToolSchema(input)
	if err != nil {
		t.Fatalf("reference-like annotation data must not be interpreted as schema: %v", err)
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(inputJSON) {
		t.Fatalf("annotation data changed: got %s, want %s", gotJSON, inputJSON)
	}
}

func TestCloneToolSchemaAcceptsBooleanNestedSchemas(t *testing.T) {
	input := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"anything": true,
			"nothing":  false,
		},
		"items": true,
	}
	if _, err := CloneToolSchema(input); err != nil {
		t.Fatalf("boolean schemas are valid JSON Schema: %v", err)
	}
}

func TestCloneToolSchemaRejectsExternalReferencesInSchemaPositions(t *testing.T) {
	external := map[string]any{"$ref": "other.json"}
	cases := map[string]map[string]any{
		"root dynamic ref":   {"type": "object", "$dynamicRef": "other.json"},
		"root recursive ref": {"type": "object", "$recursiveRef": "other.json"},
		"dependencies":       {"type": "object", "dependencies": map[string]any{"value": external}},
	}
	for _, keyword := range []string{
		"additionalItems", "additionalProperties", "contains", "contentSchema", "else", "if",
		"items", "not", "propertyNames", "then", "unevaluatedItems", "unevaluatedProperties",
	} {
		cases[keyword] = map[string]any{"type": "object", keyword: external}
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		cases[keyword] = map[string]any{"type": "object", keyword: []any{external}}
	}
	for _, keyword := range []string{"$defs", "definitions", "dependentSchemas", "patternProperties", "properties"} {
		cases[keyword] = map[string]any{"type": "object", keyword: map[string]any{"value": external}}
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := CloneToolSchema(input); err == nil || !strings.Contains(err.Error(), "external") {
				t.Fatalf("external reference error = %v", err)
			}
		})
	}
}

func TestCloneToolSchemaRejectsUnsafeSchemas(t *testing.T) {
	for name, input := range map[string]any{
		"non-object root":                      map[string]any{"type": "string"},
		"external ref":                         map[string]any{"type": "object", "$ref": "https://example.test/schema"},
		"properties not object":                map[string]any{"type": "object", "properties": []any{}},
		"property not schema":                  map[string]any{"type": "object", "properties": map[string]any{"id": "string"}},
		"required not array":                   map[string]any{"type": "object", "required": "id"},
		"required non-string":                  map[string]any{"type": "object", "required": []any{"id", 1}},
		"defs not object":                      map[string]any{"type": "object", "$defs": []any{}},
		"definition not schema":                map[string]any{"type": "object", "$defs": map[string]any{"id": "bad"}},
		"composition not array":                map[string]any{"type": "object", "oneOf": map[string]any{}},
		"composition non-schema":               map[string]any{"type": "object", "anyOf": []any{"bad"}},
		"allOf external reference":             map[string]any{"type": "object", "allOf": []any{map[string]any{"$ref": "https://example.test/schema"}}},
		"additional properties scalar":         map[string]any{"type": "object", "additionalProperties": "no"},
		"additional properties invalid schema": map[string]any{"type": "object", "additionalProperties": map[string]any{"properties": []any{}}},
		"items external reference":             map[string]any{"type": "object", "properties": map[string]any{"values": map[string]any{"type": "array", "items": map[string]any{"$ref": "https://example.test/item"}}}},
		"single schema wrong type":             map[string]any{"type": "object", "not": []any{}},
		"items array wrong type":               map[string]any{"type": "object", "items": []any{map[string]any{"type": "string"}}},
		"schema array wrong type":              map[string]any{"type": "object", "prefixItems": map[string]any{}},
		"schema array invalid entry":           map[string]any{"type": "object", "prefixItems": []any{"bad"}},
		"schema map wrong type":                map[string]any{"type": "object", "patternProperties": []any{}},
		"schema map invalid entry":             map[string]any{"type": "object", "dependentSchemas": map[string]any{"kind": "bad"}},
		"reference not string":                 map[string]any{"type": "object", "items": map[string]any{"$ref": 1}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CloneToolSchema(input)
			if err == nil {
				t.Fatal("accepted unsafe schema")
			}
		})
	}
	deep := map[string]any{"type": "object"}
	cursor := deep
	for range maxToolSchemaDepth {
		next := map[string]any{}
		cursor["nested"] = next
		cursor = next
	}
	if _, err := CloneToolSchema(deep); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("depth error = %v", err)
	}
	large := map[string]any{"type": "object", "description": strings.Repeat("x", maxToolSchemaBytes)}
	if _, err := CloneToolSchema(large); err == nil || !strings.Contains(err.Error(), "byte") {
		t.Fatalf("size error = %v", err)
	}
}
