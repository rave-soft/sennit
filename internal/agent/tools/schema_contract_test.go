package tools

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/toolmeta"
	"github.com/stretchr/testify/require"
)

// schemaContracts is deliberately exhaustive. Every row records either the
// empty strings rejected by its runtime handler, or why no such constraint is
// present. This makes adding an invalidParam check without a schema update fail
// at review time rather than silently falling back to an empty contract row.
type schemaContract struct {
	example  map[string]any
	empty    []string
	audited  bool
	reason   string
	external bool
}

var schemaContracts = map[string]schemaContract{
	"agent": {external: true, audited: true, empty: []string{"prompt"}}, "agentic_fetch": {external: true, audited: true, empty: []string{"prompt"}},
	"bash": {audited: true, empty: []string{"command"}}, "git_status": {audited: true, reason: "paths and filters are optional"}, "git_diff": {audited: true, reason: "diff options are optional"}, "git_log": {audited: true, reason: "revision and paths are optional"}, "sennit_info": {audited: true, reason: "no input"}, "sennit_logs": {audited: true, reason: "all filters are optional"}, "agent_trace": {audited: true, reason: "session_id or run_id is required"}, "job_output": {audited: true, empty: []string{"shell_id"}}, "job_kill": {audited: true, empty: []string{"shell_id"}}, "download": {audited: true, empty: []string{"url", "file_path"}}, "edit": {audited: true, empty: []string{"file_path"}}, "multiedit": {audited: true, empty: []string{"file_path"}},
	"lsp_diagnostics": {audited: true, reason: "file_path is optional"}, "lsp_references": {audited: true, empty: []string{"symbol"}}, "lsp_restart": {audited: true, reason: "no input"}, "lsp_symbols": {audited: true, empty: []string{"file_path"}}, "lsp_workspace_symbols": {audited: true, empty: []string{"query"}}, "lsp_hover": {audited: true, reason: "symbol or file position is required"}, "lsp_definition": {audited: true, empty: []string{"symbol"}}, "lsp_call_hierarchy": {audited: true, empty: []string{"symbol"}}, "lsp_rename": {audited: true, empty: []string{"symbol", "new_name"}}, "lsp_replace_symbol": {audited: true, empty: []string{"symbol", "file_path"}, example: map[string]any{"symbol": "target", "file_path": "file.go", "replacement": "new body"}},
	"fetch": {audited: true, empty: []string{"url"}}, "web_fetch": {audited: true, empty: []string{"url"}}, "web_search": {audited: true, empty: []string{"query"}}, "glob": {audited: true, empty: []string{"pattern"}}, "grep": {audited: true, empty: []string{"pattern"}}, "ripgrep": {audited: true, empty: []string{"pattern"}}, "ls": {audited: true, reason: "path is optional"},
	"question": {audited: true, reason: "nested conditional constraints are covered by TestQuestionSchemaParity", example: map[string]any{"questions": []any{map[string]any{"type": "yes_no", "question": "Continue?", "description": "Choose yes or no."}, map[string]any{"type": "single_choice", "question": "Pick one", "description": "Pick exactly one.", "choices": []any{map[string]any{"id": "one", "label": "One"}}}, map[string]any{"type": "multi_choice", "question": "Pick many", "description": "Pick any.", "options": []any{map[string]any{"id": "one", "label": "One"}}}, map[string]any{"type": "free_text", "question": "Explain", "description": "Write details.", "choices": []any{}}}}},
	"todos":    {audited: true, reason: "todo fields are intentionally optional"}, "read": {audited: true, empty: []string{"file_path"}}, "multi_read": {audited: true, reason: "files is a required bounded array; item paths are validated by each read"}, "write": {audited: true, empty: []string{"file_path"}}, "list_mcp_resources": {audited: true, empty: []string{"mcp_name"}}, "read_mcp_resource": {audited: true, empty: []string{"mcp_name", "uri"}},
	"thread_create": {audited: true, empty: []string{"name", "goal"}}, "thread_list": {audited: true, reason: "all filters are optional"}, "thread_status": {audited: true, empty: []string{"id"}}, "thread_send": {audited: true, empty: []string{"id", "message"}}, "thread_merge": {audited: true, empty: []string{"id"}}, "thread_remove": {audited: true, empty: []string{"id"}},
	"task_list": {audited: true, reason: "all filters are optional"}, "task_result": {audited: true, empty: []string{"id"}}, "task_cancel": {audited: true, empty: []string{"id"}}, "task_send": {audited: true, empty: []string{"id", "message"}}, "task_output": {audited: true, empty: []string{"id"}}, "ask_parent": {audited: true, empty: []string{"message"}},
}

func TestToolSchemaContractsAreExhaustiveAndValid(t *testing.T) {
	names := toolmeta.NamesAll()
	require.Len(t, schemaContracts, len(names), "one contract row is required per toolmeta name")
	for _, name := range names {
		contract, ok := schemaContracts[name]
		require.Truef(t, ok, "missing schema contract for %q", name)
		require.Truef(t, contract.audited, "%q must be explicitly audited", name)
		require.Truef(t, len(contract.empty) > 0 || contract.reason != "", "%q needs invalid cases or a no-constraint reason", name)
		if contract.external {
			continue
		}
		t.Run(name, func(t *testing.T) {
			tool := buildForInfo(t, name)
			require.NotNil(t, tool)
			schema := tool.Info().InputSchema
			require.Equal(t, "object", schema["type"])
			require.NotNil(t, schema["properties"])
			example := contract.example
			if example == nil {
				example = minimalObject(t, schema)
			}
			require.NoError(t, validateSchema(schema, example), "minimal valid example: %#v", example)
			for _, path := range contract.empty {
				require.Equalf(t, 1, schemaPath(t, schema, path)["minLength"], "%s must reject empty strings", path)
			}
		})
	}
}

func TestMultiReadNestedSchemaParity(t *testing.T) {
	schema := buildForInfo(t, MultiReadToolName).Info().InputSchema
	constraints := []struct {
		path, key string
		want      any
	}{
		{"files", "minItems", 1},
		{"files", "maxItems", MaxMultiReadFiles},
		{"files.items.file_path", "minLength", 1},
		{"files.items.offset", "minimum", 0},
		{"files.items.limit", "minimum", 0},
		{"files.items.limit", "maximum", DefaultReadLimit},
		{"files.items.cursor", "minLength", 1},
		{"max_bytes", "minimum", 0},
		{"max_bytes", "maximum", MaxReadSize},
		{"max_tokens", "minimum", 0},
		{"max_tokens", "maximum", MaxReadSize},
		{"cursor", "minLength", 1},
	}
	for _, constraint := range constraints {
		require.Equal(t, constraint.want, schemaPath(t, schema, constraint.path)[constraint.key], "%s/%s", constraint.path, constraint.key)
	}

	valid := map[string]any{"files": []any{map[string]any{"file_path": "file.go", "offset": 0, "limit": DefaultReadLimit, "cursor": "item"}}, "max_bytes": MaxReadSize, "max_tokens": MaxReadSize, "cursor": "batch"}
	require.NoError(t, validateSchema(schema, valid))
	for _, invalid := range []map[string]any{
		{"files": []any{}},
		{"files": []any{map[string]any{"file_path": ""}}},
		{"files": []any{map[string]any{"file_path": "file.go", "offset": -1}}},
		{"files": []any{map[string]any{"file_path": "file.go", "limit": DefaultReadLimit + 1}}},
		{"files": []any{map[string]any{"file_path": "file.go", "cursor": ""}}},
		{"files": []any{map[string]any{"file_path": "file.go"}}, "max_bytes": MaxReadSize + 1},
		{"files": []any{map[string]any{"file_path": "file.go"}}, "max_tokens": -1},
		{"files": []any{map[string]any{"file_path": "file.go"}}, "cursor": ""},
	} {
		require.Error(t, validateSchema(schema, invalid), "%#v", invalid)
	}
	tooMany := make([]any, MaxMultiReadFiles+1)
	for i := range tooMany {
		tooMany[i] = map[string]any{"file_path": "file.go"}
	}
	require.Error(t, validateSchema(schema, map[string]any{"files": tooMany}))
}

func TestQuestionSchemaParity(t *testing.T) {
	schema := buildForInfo(t, QuestionToolName).Info().InputSchema
	valid := []map[string]any{{"type": "yes_no", "question": "q", "description": "d"}, {"type": "single_choice", "question": "q", "description": "d", "choices": []any{map[string]any{"id": "a", "label": "A"}}}, {"type": "multi_choice", "question": "q", "description": "d", "options": []any{map[string]any{"id": "a", "label": "A"}}}, {"type": "single_choice", "question": "q", "description": "d", "choices": []any{map[string]any{"id": "a", "label": "A"}}, "options": []any{map[string]any{"id": "", "label": ""}}}, {"type": "free_text", "question": "q", "description": "d", "choices": []any{}}, {"type": "free_text", "question": "q", "description": "d", "options": []any{}}, {"type": "free_text", "question": "q", "description": "d", "choices": []any{map[string]any{"id": "a", "label": "A"}}, "options": []any{map[string]any{"id": "", "label": ""}}}}
	for _, item := range valid {
		require.NoError(t, validateSchema(schema, map[string]any{"questions": []any{item}}), "%#v", item)
	}
	invalid := []map[string]any{
		{"type": "single_choice", "question": "q", "description": "d"},
		{"type": "multi_choice", "question": "q", "description": "d", "options": []any{}},
		{"type": "yes_no", "question": "q", "description": "d", "choices": []any{map[string]any{"id": "a", "label": "A"}}},
		{"type": "single_choice", "question": "q", "description": "d", "choices": []any{map[string]any{"id": "", "label": "A"}}},
		{"type": "single_choice", "question": "q", "description": "d", "choices": []any{map[string]any{"id": "a", "label": ""}}},
		{"type": "multi_choice", "question": "q", "description": "d", "options": []any{map[string]any{"id": "", "label": "A"}}},
		{"type": "multi_choice", "question": "q", "description": "d", "options": []any{map[string]any{"id": "a", "label": ""}}},
		{"type": "single_choice", "question": "q", "description": "d", "choices": []any{map[string]any{"id": "", "label": ""}}, "options": []any{map[string]any{"id": "a", "label": "A"}}},
		{"type": "free_text", "question": "q", "description": "d", "choices": []any{map[string]any{"id": "", "label": "A"}}},
		{"type": "free_text", "question": "q", "description": "d", "choices": []any{map[string]any{"id": "a", "label": ""}}},
		{"type": "free_text", "question": "q", "description": "d", "options": []any{map[string]any{"id": "", "label": "A"}}},
		{"type": "free_text", "question": "q", "description": "d", "options": []any{map[string]any{"id": "a", "label": ""}}},
		{"type": "free_text", "question": "q", "description": "d", "choices": []any{map[string]any{"id": "", "label": ""}}, "options": []any{map[string]any{"id": "a", "label": "A"}}},
	}
	for _, item := range invalid {
		require.Error(t, validateSchema(schema, map[string]any{"questions": []any{item}}), "%#v", item)
	}
}

func TestReplaceSymbolSchemaParity(t *testing.T) {
	schema := buildForInfo(t, ReplaceSymbolToolName).Info().InputSchema
	base := map[string]any{"symbol": "target", "file_path": "file.go"}
	valid := []map[string]any{
		{"symbol": "target", "file_path": "file.go", "replacement": "new body"},
		{"symbol": "target", "file_path": "file.go", "action": "replace", "replacement": "new body"},
		{"symbol": "target", "file_path": "file.go", "action": "add_before", "replacement": "prefix"},
		{"symbol": "target", "file_path": "file.go", "action": "add_after", "replacement": "suffix"},
		{"symbol": "target", "file_path": "file.go", "action": "delete"},
		{"symbol": "target", "file_path": "file.go", "action": "delete", "replacement": ""},
	}
	for _, input := range valid {
		require.NoError(t, validateSchema(schema, input), "%#v", input)
	}
	invalid := []map[string]any{
		base,
		{"symbol": "target", "file_path": "file.go", "replacement": ""},
		{"symbol": "target", "file_path": "file.go", "action": "replace"},
		{"symbol": "target", "file_path": "file.go", "action": "add_before", "replacement": ""},
		{"symbol": "target", "file_path": "file.go", "action": "add_after"},
	}
	for _, input := range invalid {
		require.Error(t, validateSchema(schema, input), "%#v", input)
	}
}

func TestSennitLogsChainSchemaParity(t *testing.T) {
	schema := buildForInfo(t, SennitLogsToolName).Info().InputSchema
	for _, input := range []map[string]any{{}, {"chain": false}, {"chain": false, "session_id": " \t "}, {"chain": false, "run_id": "\t"}, {"chain": true, "session_id": "session"}, {"chain": true, "run_id": "run"}} {
		require.NoError(t, validateSchema(schema, input), "%#v", input)
	}
	for _, input := range []map[string]any{{"chain": true}, {"chain": true, "session_id": ""}, {"chain": true, "session_id": "   "}, {"chain": true, "session_id": "\t"}, {"chain": true, "run_id": ""}, {"chain": true, "run_id": " \t "}} {
		require.Error(t, validateSchema(schema, input), "%#v", input)
	}
}

func TestToolSchemaConstraintContracts(t *testing.T) {
	tests := []struct {
		tool, path, key string
		want, invalid   any
	}{{FetchToolName, "format", "enum", []any{"text", "markdown", "html"}, "TEXT"}, {FetchToolName, "timeout", "maximum", 120, 121}, {WebSearchToolName, "max_results", "maximum", 20, 21}, {DownloadToolName, "timeout", "maximum", 600, 601}, {TaskOutputToolName, "limit", "maximum", 100, 101}, {CallHierarchyToolName, "direction", "enum", []any{"incoming", "outgoing"}, "sideways"}, {ReplaceSymbolToolName, "action", "enum", []any{"replace", "add_before", "add_after", "delete"}, "move"}, {TodosToolName, "todos.items.status", "enum", []any{"pending", "in_progress", "completed"}, "unknown"}, {QuestionToolName, "questions", "minItems", 1, []any{}}, {MultiEditToolName, "edits", "minItems", 1, []any{}}, {GrepToolName, "max_results", "minimum", 0, -1}, {RipgrepToolName, "max_results", "minimum", 0, -1}, {GlobToolName, "max_results", "minimum", 0, -1}, {ReadToolName, "limit", "minimum", 0, -1}, {SennitLogsToolName, "level", "enum", []any{"DEBUG", "INFO", "WARN", "ERROR"}, "info"}, {GitStatusToolName, "limit", "minimum", 0, -1}, {GitStatusToolName, "limit", "maximum", 1000, 1001}, {GitDiffToolName, "mode", "enum", []any{"unstaged", "staged", "revision"}, "all"}, {GitDiffToolName, "format", "enum", []any{"patch", "stat"}, "raw"}, {GitDiffToolName, "max_bytes", "minimum", 0, -1}, {GitDiffToolName, "max_bytes", "maximum", 200000, 200001}, {GitLogToolName, "limit", "minimum", 0, -1}, {GitLogToolName, "limit", "maximum", 200, 201}}
	for _, test := range tests {
		t.Run(test.tool+"/"+test.path, func(t *testing.T) {
			parameter := schemaPath(t, buildForInfo(t, test.tool).Info().InputSchema, test.path)
			require.True(t, reflect.DeepEqual(test.want, parameter[test.key]))
			require.Error(t, validateSchema(parameter, test.invalid))
		})
	}
}

// validateSchema is a focused test-only JSON Schema evaluator for the keywords
// advertised by tool schemas. Keywords compose: anyOf never skips sibling
// constraints, and object properties/required are evaluated independently of a
// type declaration, as required by JSON Schema.
func validateSchema(schema map[string]any, value any) error {
	if not, ok := schema["not"].(map[string]any); ok && validateSchema(not, value) == nil {
		return fmt.Errorf("matches forbidden schema")
	}
	if alternatives, ok := schema["anyOf"].([]any); ok {
		matched := false
		for _, alternative := range alternatives {
			if candidate, ok := alternative.(map[string]any); ok && validateSchema(candidate, value) == nil {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("does not satisfy anyOf")
		}
	}
	if allOf, ok := schema["allOf"].([]any); ok {
		for _, conjunct := range allOf {
			if candidate, ok := conjunct.(map[string]any); ok {
				if err := validateSchema(candidate, value); err != nil {
					return err
				}
			}
		}
	}
	if condition, ok := schema["if"].(map[string]any); ok && validateSchema(condition, value) == nil {
		if then, ok := schema["then"].(map[string]any); ok {
			if err := validateSchema(then, value); err != nil {
				return err
			}
		}
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, item := range enum {
			found = found || reflect.DeepEqual(item, value)
		}
		if !found {
			return fmt.Errorf("not in enum")
		}
	}
	if constant, ok := schema["const"]; ok && !reflect.DeepEqual(constant, value) {
		return fmt.Errorf("does not match const")
	}
	if properties, hasProperties := schema["properties"].(map[string]any); hasProperties || schema["required"] != nil {
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("expected object")
		}
		for _, name := range stringSlice(schema["required"]) {
			if _, ok := object[name]; !ok {
				return fmt.Errorf("missing required %q", name)
			}
		}
		for name, child := range properties {
			if input, exists := object[name]; exists {
				if nested, ok := child.(map[string]any); ok {
					if err := validateSchema(nested, input); err != nil {
						return fmt.Errorf("%s: %w", name, err)
					}
				}
			}
		}
	}
	switch schema["type"] {
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("expected object")
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("expected array")
		}
		if err := numericBounds(schema, len(array)); err != nil {
			return err
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for _, input := range array {
				if err := validateSchema(items, input); err != nil {
					return err
				}
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("expected string")
		}
		if minimum, ok := schema["minLength"].(int); ok && len([]rune(text)) < minimum {
			return fmt.Errorf("shorter than minLength")
		}
		if pattern, ok := schema["pattern"].(string); ok {
			re, err := regexp.Compile(pattern)
			if err != nil || !re.MatchString(text) {
				return fmt.Errorf("does not match pattern")
			}
		}
	case "integer", "number":
		number, ok := asInt(value)
		if !ok {
			return fmt.Errorf("expected number")
		}
		if err := numericBounds(schema, number); err != nil {
			return err
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("expected boolean")
		}
	}
	return nil
}

func numericBounds(schema map[string]any, value int) error {
	if minimum, ok := schema["minimum"].(int); ok && value < minimum {
		return fmt.Errorf("below minimum")
	}
	if maximum, ok := schema["maximum"].(int); ok && value > maximum {
		return fmt.Errorf("above maximum")
	}
	if minimum, ok := schema["minItems"].(int); ok && value < minimum {
		return fmt.Errorf("below minItems")
	}
	if maximum, ok := schema["maxItems"].(int); ok && value > maximum {
		return fmt.Errorf("above maxItems")
	}
	return nil
}

func asInt(value any) (int, bool) {
	switch n := value.(type) {
	case int:
		return n, true
	case float64:
		return int(n), float64(int(n)) == n
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func stringSlice(value any) []string {
	if raw, ok := value.([]string); ok {
		return raw
	}
	values, _ := value.([]any)
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func minimalObject(t *testing.T, schema map[string]any) map[string]any {
	t.Helper()
	out := map[string]any{}
	properties, _ := schema["properties"].(map[string]any)
	for _, name := range stringSlice(schema["required"]) {
		property, ok := properties[name].(map[string]any)
		require.Truef(t, ok, "required %q has no schema", name)
		out[name] = minimalValue(t, property)
	}
	return out
}

func minimalValue(t *testing.T, schema map[string]any) any {
	t.Helper()
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}
	switch schema["type"] {
	case "object":
		return minimalObject(t, schema)
	case "array":
		minimum, _ := schema["minItems"].(int)
		values := make([]any, minimum)
		item, _ := schema["items"].(map[string]any)
		for i := range values {
			values[i] = minimalValue(t, item)
		}
		return values
	case "string":
		minimum, _ := schema["minLength"].(int)
		return strings.Repeat("x", max(1, minimum))
	case "integer", "number":
		minimum, _ := schema["minimum"].(int)
		return minimum
	case "boolean":
		return false
	}
	return nil
}

func schemaPath(t *testing.T, schema map[string]any, path string) map[string]any {
	t.Helper()
	parts := strings.Split(path, ".")
	current, ok := schema["properties"].(map[string]any)[parts[0]]
	require.Truef(t, ok, "schema missing root %q", parts[0])
	for _, part := range parts[1:] {
		object, ok := current.(map[string]any)
		require.Truef(t, ok, "schema path %q is not an object", path)
		if part == "items" {
			current = object["items"]
		} else {
			properties, ok := object["properties"].(map[string]any)
			require.Truef(t, ok, "schema path %q has no properties", path)
			current = properties[part]
		}
		require.NotNilf(t, current, "schema path %q missing %q", path, part)
	}
	parameter, ok := current.(map[string]any)
	require.Truef(t, ok, "schema path %q is not an object: %T", path, current)
	return parameter
}
