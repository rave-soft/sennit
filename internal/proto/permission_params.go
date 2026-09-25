package proto

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
)

// WebFetchPermissionsParams represents the permission parameters for the
// web_fetch tool. It is defined here, in the leaf package, and aliased
// from internal/agent/tools; see the comment atop tools.go.
type WebFetchPermissionsParams struct {
	URL string `json:"url"`
}

// WebSearchPermissionsParams represents the permission parameters for the
// web_search tool. It is defined here, in the leaf package, and aliased
// from internal/agent/tools; see the comment atop tools.go.
type WebSearchPermissionsParams struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results,omitempty"`
}

// AgentCancelPermissionParams represents the permission parameters for the
// agent_cancel tool. It is defined here, in the leaf package, and aliased
// from internal/agent/tools; see the comment atop tools.go.
type AgentCancelPermissionParams struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// ReadMCPResourcePermissionsParams represents the permission parameters
// for the read_mcp_resource tool. It is defined here, in the leaf
// package, and aliased from internal/agent/tools; see the comment atop
// tools.go.
type ReadMCPResourcePermissionsParams struct {
	MCPName string `json:"mcp_name"`
	URI     string `json:"uri"`
}

// ListMCPResourcesPermissionsParams represents the permission parameters
// for the list_mcp_resources tool. It is defined here, in the leaf
// package, and aliased from internal/agent/tools; see the comment atop
// tools.go.
type ListMCPResourcesPermissionsParams struct {
	MCPName string `json:"mcp_name"`
}

// RenamePermissionsParams represents the permission parameters for the
// lsp_rename tool. It is defined here, in the leaf package, and aliased
// from internal/agent/tools; see the comment atop tools.go.
//
// lsp_rename's tool-argument type, tools.RenameParams, cannot fill this
// role itself the way BashPermissionsParams does for bash: its json
// struct tags also carry the "description" the model-facing tool schema
// reads (see third_party/fantasy's schema reflection), and RenameParams
// (this package's own, unrelated, currently-unused DTO at ui_tools.go)
// has no such tags either, so aliasing either one would either strip the
// schema or reuse a type meant for something else. lsp_rename instead
// converts its own params into this type before raising the request, the
// same way edit.go turns EditParams into EditPermissionsParams.
type RenamePermissionsParams struct {
	Symbol  string `json:"symbol"`
	NewName string `json:"new_name"`
	Path    string `json:"path,omitempty"`
}

// permissionParamsRegistry maps a tool name to a constructor for the
// concrete Params type that tool passes to permission.CreatePermissionRequest
// today (see each tool's own CreatePermissionRequest{... Params: ...}
// call site in internal/agent and internal/agent/tools). It exists so a
// permission.PermissionRequest decoded from JSON - which is what every
// request becomes once the Workspace is served over a wire, and already
// happens whenever one round-trips through Go's encoding/json - lands
// back in the same concrete type the dialog's renderer registry
// (internal/ui/dialog/permissions.go) type-asserts on, instead of the
// map[string]any a plain `any` field decodes into.
//
// "read" and "multi_read" share ReadPermissionsParams because both tools
// raise their outside-workdir request through the same
// requireOutsideWorkdirPermission call in internal/agent/tools/read_core.go.
var permissionParamsRegistry = map[string]func() any{
	BashToolName:             func() any { return &BashPermissionsParams{} },
	EditToolName:             func() any { return &EditPermissionsParams{} },
	WriteToolName:            func() any { return &WritePermissionsParams{} },
	MultiEditToolName:        func() any { return &MultiEditPermissionsParams{} },
	ReplaceSymbolToolName:    func() any { return &ReplaceSymbolPermissionsParams{} },
	DownloadToolName:         func() any { return &DownloadPermissionsParams{} },
	FetchToolName:            func() any { return &FetchPermissionsParams{} },
	AgenticFetchToolName:     func() any { return &AgenticFetchPermissionsParams{} },
	ReadToolName:             func() any { return &ReadPermissionsParams{} },
	MultiReadToolName:        func() any { return &ReadPermissionsParams{} },
	LSToolName:               func() any { return &LSPermissionsParams{} },
	GlobToolName:             func() any { return &GlobPermissionsParams{} },
	GrepToolName:             func() any { return &GrepPermissionsParams{} },
	RipgrepToolName:          func() any { return &RipgrepPermissionsParams{} },
	WebFetchToolName:         func() any { return &WebFetchPermissionsParams{} },
	WebSearchToolName:        func() any { return &WebSearchPermissionsParams{} },
	AgentCancelToolName:      func() any { return &AgentCancelPermissionParams{} },
	ListMCPResourcesToolName: func() any { return &ListMCPResourcesPermissionsParams{} },
	ReadMCPResourceToolName:  func() any { return &ReadMCPResourcePermissionsParams{} },
	RenameToolName:           func() any { return &RenamePermissionsParams{} },
}

// DecodePermissionParams decodes a permission request's raw "params" JSON
// into the concrete type the named tool passes today, so a
// permission.PermissionRequest surviving a JSON round trip - the shape
// every request will take once the Workspace is served over a wire -
// renders the same as one built in-process.
//
//   - A tool name this registry recognizes decodes into that tool's
//     PermissionsParams struct, returned by value (the same kind every
//     tool passes at its call site, and what the dialog's renderer
//     registry type-asserts on).
//   - An MCP tool ("mcp_<server>_<tool>", see SplitMCPToolName) decodes
//     into a plain string: that is what mcp-tools.go passes today
//     (params.Input, a fantasy.ToolCall.Input already-JSON string).
//   - Any other tool name decodes into map[string]any, exactly what the
//     zero-value `any` field this replaces produced before this registry
//     existed, and is logged once at Debug so an unregistered tool that
//     starts raising requests is discoverable without breaking decoding.
func DecodePermissionParams(toolName string, raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	if strings.HasPrefix(toolName, MCPToolNamePrefix) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("decode mcp permission params for %q: %w", toolName, err)
		}
		return s, nil
	}

	factory, ok := permissionParamsRegistry[toolName]
	if !ok {
		slog.Debug("Decoding permission params for unregistered tool", "tool_name", toolName)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("decode permission params for %q: %w", toolName, err)
		}
		return m, nil
	}

	ptr := factory()
	if err := json.Unmarshal(raw, ptr); err != nil {
		return nil, fmt.Errorf("decode permission params for %q: %w", toolName, err)
	}
	return reflect.ValueOf(ptr).Elem().Interface(), nil
}
