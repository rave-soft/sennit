package proto

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDecodePermissionParams_Registered covers a representative sample of
// the registry - the full table lives in
// internal/agent/tools/permission_params_registry_test.go, which is the
// one place every call site is enumerated and cross-checked against this
// package's registry.
func TestDecodePermissionParams_Registered(t *testing.T) {
	t.Parallel()

	got, err := DecodePermissionParams(BashToolName, []byte(`{"command":"echo hi","working_dir":"/tmp"}`))
	require.NoError(t, err)
	require.Equal(t, BashPermissionsParams{Command: "echo hi", WorkingDir: "/tmp"}, got)

	got, err = DecodePermissionParams(EditToolName, []byte(`{"file_path":"a.go","new_content":"x"}`))
	require.NoError(t, err)
	require.Equal(t, EditPermissionsParams{FilePath: "a.go", NewContent: "x"}, got)
}

// TestDecodePermissionParams_MCP covers the dynamic mcp_<server>_<tool>
// case: Params stays a plain string, matching what mcp-tools.go passes
// today (params.Input).
func TestDecodePermissionParams_MCP(t *testing.T) {
	t.Parallel()

	got, err := DecodePermissionParams("mcp_myserver_do_thing", []byte(`"{\"arg\":1}"`))
	require.NoError(t, err)
	require.Equal(t, `{"arg":1}`, got)
}

// TestDecodePermissionParams_Unknown covers a tool name the registry does
// not recognize: it decodes into map[string]any (what a plain `any`
// field produced before this registry existed) rather than erroring.
func TestDecodePermissionParams_Unknown(t *testing.T) {
	t.Parallel()

	got, err := DecodePermissionParams("some_future_tool", []byte(`{"foo":"bar"}`))
	require.NoError(t, err)
	require.Equal(t, map[string]any{"foo": "bar"}, got)
}

// TestDecodePermissionParams_Empty covers the absent/explicit-null cases
// permission.PermissionRequest.UnmarshalJSON relies on to distinguish "no
// params key" from "params explicitly cleared".
func TestDecodePermissionParams_Empty(t *testing.T) {
	t.Parallel()

	got, err := DecodePermissionParams(BashToolName, nil)
	require.NoError(t, err)
	require.Nil(t, got)

	got, err = DecodePermissionParams(BashToolName, []byte(`null`))
	require.NoError(t, err)
	require.Nil(t, got)
}

// TestDecodePermissionParams_InvalidJSON covers a malformed raw payload:
// it must return an error, not panic or silently drop data.
func TestDecodePermissionParams_InvalidJSON(t *testing.T) {
	t.Parallel()

	_, err := DecodePermissionParams(BashToolName, []byte(`{not json`))
	require.Error(t, err)

	_, err = DecodePermissionParams("some_future_tool", []byte(`{not json`))
	require.Error(t, err)

	_, err = DecodePermissionParams("mcp_myserver_do_thing", []byte(`{not json`))
	require.Error(t, err)
}

// TestPermissionParamsRegistry_NoMCPPrefix guards against a registry
// entry that would collide with SplitMCPToolName's routing: nothing
// registered here should ever start with "mcp_", or
// DecodePermissionParams's prefix check would shadow it. This reads
// permissionParamsRegistry directly rather than through an exported
// accessor built only for this test - internal/agent/tools' own
// completeness check (permission_params_registry_test.go) covers whether
// every call site is registered; a registry entry this package's own
// test doesn't separately name is not a gap that check needs to expose.
func TestPermissionParamsRegistry_NoMCPPrefix(t *testing.T) {
	t.Parallel()

	for name := range permissionParamsRegistry {
		require.False(t, strings.HasPrefix(name, MCPToolNamePrefix),
			"registered tool name %q collides with the mcp_ prefix", name)
	}
}
