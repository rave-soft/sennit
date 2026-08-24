package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// These limits keep untrusted MCP metadata from consuming disproportionate
// memory while retaining schemas large enough for normal generated APIs.
const (
	maxToolSchemaBytes = 256 << 10
	maxToolSchemaDepth = 64
)

var (
	singleSchemaKeywords = map[string]struct{}{
		"additionalItems": {}, "additionalProperties": {}, "contains": {},
		"contentSchema": {}, "else": {}, "if": {}, "items": {}, "not": {},
		"propertyNames": {}, "then": {}, "unevaluatedItems": {}, "unevaluatedProperties": {},
	}
	schemaArrayKeywords = map[string]struct{}{
		"allOf": {}, "anyOf": {}, "oneOf": {}, "prefixItems": {},
	}
	schemaMapKeywords = map[string]struct{}{
		"$defs": {}, "definitions": {}, "dependentSchemas": {},
		"patternProperties": {}, "properties": {},
	}
)

func validateToolSchemas(tools []*Tool) error {
	for _, tool := range tools {
		if _, err := CloneToolSchema(tool.InputSchema); err != nil {
			return fmt.Errorf("MCP tool %q has invalid input schema: %w", tool.Name, err)
		}
	}
	return nil
}

// CloneToolSchema validates only the transport safety contract. It deliberately
// does not resolve refs: MCP schemas are forwarded as supplied and may refer
// only to their own document. It returns an independent schema for consumers.
func CloneToolSchema(value any) (map[string]any, error) {
	schema, ok := value.(map[string]any)
	if !ok || schema == nil {
		return nil, fmt.Errorf("root must be an object")
	}
	if typ, exists := schema["type"]; exists && typ != "object" {
		return nil, fmt.Errorf("root type must be object")
	}
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("not JSON serializable: %w", err)
	}
	if len(data) > maxToolSchemaBytes {
		return nil, fmt.Errorf("exceeds %d-byte limit", maxToolSchemaBytes)
	}
	var cloned map[string]any
	if err := json.Unmarshal(data, &cloned); err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}
	if err := inspectSchemaDepth(cloned, 1); err != nil {
		return nil, err
	}
	if err := inspectSchema(cloned, 1); err != nil {
		return nil, err
	}
	return cloned, nil
}

func inspectSchemaDepth(value any, depth int) error {
	if depth > maxToolSchemaDepth {
		return fmt.Errorf("exceeds %d-level depth limit", maxToolSchemaDepth)
	}
	switch value := value.(type) {
	case map[string]any:
		for _, child := range value {
			if err := inspectSchemaDepth(child, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := inspectSchemaDepth(child, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func inspectSchema(value any, depth int) error {
	if depth > maxToolSchemaDepth {
		return fmt.Errorf("exceeds %d-level depth limit", maxToolSchemaDepth)
	}
	if _, ok := value.(bool); ok {
		return nil
	}
	schema, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("schema must be an object or boolean")
	}
	for _, keyword := range []string{"$ref", "$dynamicRef", "$recursiveRef"} {
		value, exists := schema[keyword]
		if !exists {
			continue
		}
		ref, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s must be a string", keyword)
		}
		if !strings.HasPrefix(ref, "#") {
			return fmt.Errorf("external %s is not allowed", keyword)
		}
	}
	if required, exists := schema["required"]; exists {
		values, ok := required.([]any)
		if !ok {
			return fmt.Errorf("required must be an array of strings")
		}
		for _, value := range values {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("required must be an array of strings")
			}
		}
	}
	for keyword, value := range schema {
		if _, ok := singleSchemaKeywords[keyword]; ok {
			if err := inspectSchema(value, depth+1); err != nil {
				return fmt.Errorf("%s: %w", keyword, err)
			}
			continue
		}
		if _, ok := schemaArrayKeywords[keyword]; ok {
			children, ok := value.([]any)
			if !ok {
				return fmt.Errorf("%s must be an array of schemas", keyword)
			}
			if err := inspectSchemaChildren(children, depth); err != nil {
				return fmt.Errorf("%s: %w", keyword, err)
			}
			continue
		}
		if _, ok := schemaMapKeywords[keyword]; ok {
			children, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("%s must be an object", keyword)
			}
			for name, child := range children {
				if err := inspectSchema(child, depth+1); err != nil {
					return fmt.Errorf("%s.%s: %w", keyword, name, err)
				}
			}
			continue
		}
		if keyword == "dependencies" {
			if err := inspectDependencies(value, depth); err != nil {
				return err
			}
		}
	}
	return nil
}

func inspectSchemaChildren(children []any, depth int) error {
	for index, child := range children {
		if err := inspectSchema(child, depth+1); err != nil {
			return fmt.Errorf("[%d]: %w", index, err)
		}
	}
	return nil
}

func inspectDependencies(value any, depth int) error {
	dependencies, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("dependencies must be an object")
	}
	for name, dependency := range dependencies {
		if names, ok := dependency.([]any); ok {
			for _, value := range names {
				if _, ok := value.(string); !ok {
					return fmt.Errorf("dependencies.%s must be a schema or an array of strings", name)
				}
			}
			continue
		}
		if err := inspectSchema(dependency, depth+1); err != nil {
			return fmt.Errorf("dependencies.%s: %w", name, err)
		}
	}
	return nil
}
