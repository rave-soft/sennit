package tools

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/stretchr/testify/require"
)

// permissionParamsSite documents one place in this package that raises a
// permission request with a concrete, non-MCP Params type, and the type
// proto.DecodePermissionParams must reproduce for it. It was built by
// enumerating every `permission.CreatePermissionRequest{` and
// `requireOutsideWorkdirPermission(` call site in this package (see the
// grep commands in TestPermissionParamsSitesCoverKnownCallSites' own
// comment) plus the one site outside it,
// internal/agent/delegation_finalizer.go, which raises agentic_fetch with
// the same AgenticFetchPermissionsParams tools already aliases from
// proto.
//
// A go/packages, type-checked AST walk would verify this table without
// hand maintenance, but it is a heavy dependency for a test and slow to
// run; TestPermissionParamsSitesCoverKnownCallSites' call-site count is
// the cheaper alternative this file settles for; it does not resolve tool
// names or types by itself, but it fails the moment a new call site
// appears, which forces whoever added it to extend this table (and the
// registry) rather than discovering the gap from a silently-wrong
// permission dialog.
var permissionParamsSites = []struct {
	toolName string
	params   any
}{
	{BashToolName, BashPermissionsParams{}},
	{EditToolName, EditPermissionsParams{}},
	{WriteToolName, WritePermissionsParams{}},
	{MultiEditToolName, MultiEditPermissionsParams{}},
	{ReplaceSymbolToolName, ReplaceSymbolPermissionsParams{}},
	{DownloadToolName, DownloadPermissionsParams{}},
	{FetchToolName, FetchPermissionsParams{}},
	{AgenticFetchToolName, AgenticFetchPermissionsParams{}}, // raised in internal/agent, not here
	{ReadToolName, ReadPermissionsParams{}},
	{MultiReadToolName, ReadPermissionsParams{}},
	{LSToolName, LSPermissionsParams{}},
	{GlobToolName, GlobPermissionsParams{}},
	{GrepToolName, GrepPermissionsParams{}},
	{RipgrepToolName, RipgrepPermissionsParams{}},
	{WebFetchToolName, WebFetchPermissionsParams{}},
	{WebSearchToolName, WebSearchPermissionsParams{}},
	{AgentCancelToolName, AgentCancelPermissionParams{}},
	{ListMCPResourcesToolName, ListMCPResourcesPermissionsParams{}},
	{ReadMCPResourceToolName, ReadMCPResourcePermissionsParams{}},
	// lsp_rename converts its own RenameParams into RenamePermissionsParams
	// before raising the request (lsp_rename.go); see
	// TestLSPRenamePermissionParamsType and
	// TestLSPRenameThroughManagerRequestsOperationScopedPermission
	// (lsp_manager_e2e_test.go), which together pin that conversion at
	// the source and through the real permission path, so a regression to
	// passing the tool's raw argument struct (RenameParams, which keeps
	// its own description tags for the model-facing schema and so cannot
	// itself be the wire type) is caught even though this table cannot
	// see the real call site's value.
	{RenameToolName, RenamePermissionsParams{}},
}

// createPermissionRequestPattern and requireOutsideWorkdirPermissionPattern
// count this package's call sites the same way permissionAction (in
// permission_actions_test.go) counts Action literals: a plain grep over
// the package's own non-test source, so a new site changes the count and
// fails the test until permissionParamsSites is updated to match.
var (
	createPermissionRequestPattern      = regexp.MustCompile(`permission\.CreatePermissionRequest\{`)
	requireOutsideWorkdirPermissionCall = regexp.MustCompile(`(^|[^\w])requireOutsideWorkdirPermission\(`)
	requireOutsideWorkdirPermissionFunc = regexp.MustCompile(`^func requireOutsideWorkdirPermission\(`)
)

// TestPermissionParamsSitesCoverKnownCallSites is the drift guard
// described on permissionParamsSites: it does not know *which* tool name
// or Params type a new call site would use, only that one appeared. Two
// call-site shapes raise a request in this package:
//
//   - `permission.CreatePermissionRequest{...}` directly - one literal
//     per distinct Params type, except filemutation.go's single shared
//     literal (edit/write/multiedit/replace_symbol, 4 tool names) and
//     mcp-tools.go's dynamic mcp_* tool (Params is the raw string
//     params.Input, handled by DecodePermissionParams's MCP prefix check
//     rather than a table entry).
//   - `requireOutsideWorkdirPermission(...)` - one call site per
//     ls/glob/ripgrep/grep, plus read_core.go's single call site used by
//     both read and multi_read (2 tool names, 1 site).
func TestPermissionParamsSitesCoverKnownCallSites(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	var createSites, outsideSites int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		contents, err := os.ReadFile(name)
		require.NoError(t, err)
		createSites += len(createPermissionRequestPattern.FindAll(contents, -1))
		for _, line := range strings.Split(string(contents), "\n") {
			if requireOutsideWorkdirPermissionFunc.MatchString(line) {
				continue // the helper's own definition, not a call site
			}
			if requireOutsideWorkdirPermissionCall.MatchString(line) {
				outsideSites++
			}
		}
	}

	// 12 CreatePermissionRequest{ literals: agent_cancel, fetch,
	// list_mcp_resources, mcp-tools (dynamic), lsp_rename, download,
	// bash, filemutation (shared by 4 tool names), tools.go (the
	// requireOutsideWorkdirPermission helper's own literal),
	// read_mcp_resource, web_fetch, web_search.
	require.Equal(t, 12, createSites,
		"a permission.CreatePermissionRequest{ call site was added or removed in this package; "+
			"update permissionParamsSites (and the registry in internal/proto) to match")
	// 5 requireOutsideWorkdirPermission( call sites: ls, glob, ripgrep,
	// grep, read_core (shared by read and multi_read).
	require.Equal(t, 5, outsideSites,
		"a requireOutsideWorkdirPermission( call site was added or removed in this package; "+
			"update permissionParamsSites (and the registry in internal/proto) to match")
}

// TestPermissionParamsSitesMatchRegistry is the forward check: every site
// this table names decodes, through proto.DecodePermissionParams, into
// exactly the type the table says. It intentionally does not check the
// reverse direction (does the registry have an entry nothing here names)
// - that would need proto to expose its internal tool-name set for a
// test-only reason, the pattern this repo removed in 59666e391. A
// registry entry this table doesn't cover is harmless on its own (it just
// means some tool's requests decode correctly without this package's
// table saying so); TestPermissionParamsSitesCoverKnownCallSites' call-site
// count is what actually catches a tool this package added without
// registering it.
func TestPermissionParamsSitesMatchRegistry(t *testing.T) {
	t.Parallel()

	for _, site := range permissionParamsSites {
		t.Run(site.toolName, func(t *testing.T) {
			t.Parallel()

			raw, err := json.Marshal(site.params)
			require.NoError(t, err)

			got, err := proto.DecodePermissionParams(site.toolName, raw)
			require.NoError(t, err)
			require.IsType(t, site.params, got,
				"proto.DecodePermissionParams(%q, ...) returned the wrong concrete type", site.toolName)
		})
	}
}

// lspRenamePermissionParamsPattern pins the fix for lsp_rename raising a
// permission request with two different Go types depending on whether
// the request went through JSON: in-process it used to carry
// tools.RenameParams (the tool's own argument struct - kept local
// because its struct tags also feed the model-facing tool schema, so it
// cannot itself become the wire type), while a JSON round trip decoded
// into proto.RenameParams, an unrelated type. lsp_rename.go now converts
// its params into RenamePermissionsParams before raising the request, so
// both paths agree.
//
// permissionParamsSites (and TestPermissionParamsSitesMatchRegistry
// above) can only assert what proto.DecodePermissionParams itself
// produces; they cannot see what value lsp_rename.go's own
// CreatePermissionRequest call site actually constructs. This pins that
// literal directly and runs in a few milliseconds, as a fast, permanent
// signal alongside the real thing:
// TestLSPRenameThroughManagerRequestsOperationScopedPermission
// (lsp_manager_e2e_test.go) drives the actual tool through a real LSP
// fixture and asserts the published PermissionRequest's Params equals a
// RenamePermissionsParams value byte-for-byte - the authoritative check,
// costed here (it re-execs a helper process) because that fixture already
// exists and is exercised elsewhere in this package for lsp_rename's
// other behavior.
var lspRenamePermissionParamsPattern = regexp.MustCompile(`Params:\s*RenamePermissionsParams\{`)

func TestLSPRenamePermissionParamsType(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile("lsp_rename.go")
	require.NoError(t, err)
	require.True(t, lspRenamePermissionParamsPattern.Match(contents),
		"lsp_rename.go's CreatePermissionRequest must pass RenamePermissionsParams, "+
			"not the tool's own RenameParams argument struct (see this test's doc comment)")
}

// populateNonZero fills every exported field of a struct (recursively,
// for nested structs) with a distinct non-zero value, so a round-trip
// test can't pass by accident on a Params value that was zero all along.
func populateNonZero(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString("x-" + path)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		populateNonZero(t, elem, path+"[0]")
		v.Set(reflect.Append(v, elem))
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Field(i)
			if !f.CanSet() {
				continue
			}
			populateNonZero(t, f, path+"."+v.Type().Field(i).Name)
		}
	default:
		t.Fatalf("populateNonZero: unhandled kind %s for %s (add a case for the new field type)", v.Kind(), path)
	}
}

// requireNoZeroExportedField is the reflection guard the task asked for:
// it fails if populateNonZero left any exported field at its zero value,
// which would let a round-trip test pass without actually exercising
// that field.
func requireNoZeroExportedField(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Field(i)
			ft := v.Type().Field(i)
			if !ft.IsExported() {
				continue
			}
			requireNoZeroExportedField(t, f, path+"."+ft.Name)
		}
	default:
		require.False(t, v.IsZero(), "%s is zero after populateNonZero", path)
	}
}

// TestPermissionParamsRoundTrip is the acceptance criterion's round-trip
// test: for every registered tool, a fully populated PermissionRequest
// survives json.Marshal -> json.Unmarshal with the exact concrete Params
// type preserved, not map[string]any.
func TestPermissionParamsRoundTrip(t *testing.T) {
	t.Parallel()

	for _, site := range permissionParamsSites {
		t.Run(site.toolName, func(t *testing.T) {
			t.Parallel()

			params := reflect.New(reflect.TypeOf(site.params)).Elem()
			populateNonZero(t, params, "Params")
			requireNoZeroExportedField(t, params, "Params")

			req := permission.PermissionRequest{
				ID:          "perm-" + site.toolName,
				SessionID:   "session-1",
				ToolCallID:  "call-1",
				ToolName:    site.toolName,
				Description: "round trip for " + site.toolName,
				Action:      "execute",
				Params:      params.Interface(),
				Path:        "/workspace",
			}

			encoded, err := json.Marshal(req)
			require.NoError(t, err)

			var decoded permission.PermissionRequest
			require.NoError(t, json.Unmarshal(encoded, &decoded))

			require.IsType(t, site.params, decoded.Params,
				"tool %q decoded into the wrong concrete type", site.toolName)
			require.Equal(t, req.Params, decoded.Params)
			require.Equal(t, req.ID, decoded.ID)
			require.Equal(t, req.ToolName, decoded.ToolName)
		})
	}
}

// TestPermissionParamsRoundTrip_MCP covers the dynamic mcp_<server>_<tool>
// case: Params stays a plain string, exactly what mcp-tools.go passes
// (params.Input, a fantasy.ToolCall.Input already-JSON string) - never
// decoded a second time into a struct or map.
func TestPermissionParamsRoundTrip_MCP(t *testing.T) {
	t.Parallel()

	req := permission.PermissionRequest{
		ID:       "perm-mcp",
		ToolName: "mcp_myserver_do_thing",
		Params:   `{"arg":"value"}`,
	}
	encoded, err := json.Marshal(req)
	require.NoError(t, err)

	var decoded permission.PermissionRequest
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, `{"arg":"value"}`, decoded.Params)
}

// TestPermissionParamsRoundTrip_UnknownTool covers a tool name the
// registry does not recognize: Params decodes into map[string]any, the
// same shape a plain `any` field produced before this registry existed,
// so an unrecognized tool degrades to the old generic rendering instead
// of failing to decode.
func TestPermissionParamsRoundTrip_UnknownTool(t *testing.T) {
	t.Parallel()

	req := permission.PermissionRequest{
		ID:       "perm-unknown",
		ToolName: "some_future_tool",
		Params:   map[string]any{"foo": "bar", "count": float64(3)},
	}
	encoded, err := json.Marshal(req)
	require.NoError(t, err)

	var decoded permission.PermissionRequest
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, map[string]any{"foo": "bar", "count": float64(3)}, decoded.Params)
}

// TestPermissionParamsSites_NoDuplicateToolNames guards the table itself:
// a copy-paste duplicate would silently shadow one of the two intended
// entries, and nothing else here would notice, so this checks the slice
// directly.
func TestPermissionParamsSites_NoDuplicateToolNames(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, len(permissionParamsSites))
	for _, site := range permissionParamsSites {
		require.False(t, seen[site.toolName], "duplicate tool name %q in permissionParamsSites", site.toolName)
		seen[site.toolName] = true
	}
}
