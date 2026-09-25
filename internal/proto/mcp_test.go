package proto_test

import (
	"encoding/json"
	"testing"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/stretchr/testify/require"
)

// TestMCPState_JSONRoundTrips pins a defect the workspace package's PR 0.3
// wire-DTO test found: MCPState implements MarshalText but had no
// UnmarshalText, so encoding/json encoded it as a string ("connected")
// but then tried to decode that same string straight into the
// underlying int and failed outright - any struct carrying an MCPState
// field marshaled fine and then could never be unmarshaled back.
func TestMCPState_JSONRoundTrips(t *testing.T) {
	t.Parallel()

	for _, want := range []proto.MCPState{
		proto.MCPStateDisabled,
		proto.MCPStateStarting,
		proto.MCPStateConnected,
		proto.MCPStateError,
		proto.MCPStateNeedsAuth,
	} {
		data, err := json.Marshal(want)
		require.NoError(t, err)

		var got proto.MCPState
		require.NoError(t, json.Unmarshal(data, &got))
		require.Equal(t, want, got)
	}
}

// TestMCPState_UnmarshalUnknownDegradesToDisabled pins UnmarshalText's
// forward-compatibility behavior: a state value from a newer build this
// one does not recognize decodes to MCPStateDisabled instead of failing
// the whole decode.
func TestMCPState_UnmarshalUnknownDegradesToDisabled(t *testing.T) {
	t.Parallel()

	var got proto.MCPState
	require.NoError(t, json.Unmarshal([]byte(`"some future state"`), &got))
	require.Equal(t, proto.MCPStateDisabled, got)
}

// TestSplitMCPToolName_ServerNameWithUnderscore pins Audit 12 finding 5:
// an MCP server's name is a config key, not a generated identifier, and
// may contain underscores. The naive "first two underscores" split this
// used to do reads "mcp_my_server_do_thing" as server="my", tool="server_do_thing"
// — wrong on both sides. Given the real server name, the split must
// resolve to the true boundary instead.
func TestSplitMCPToolName_ServerNameWithUnderscore(t *testing.T) {
	t.Parallel()

	server, tool, ok := proto.SplitMCPToolName("mcp_my_server_do_thing", []string{"my_server"})
	require.True(t, ok)
	require.Equal(t, "my_server", server)
	require.Equal(t, "do_thing", tool)
}

// TestSplitMCPToolName_PrefersLongestKnownServer pins the "my" vs.
// "my_server" ambiguity called out in the doc comment: when both are
// configured, the longer, more specific name must win.
func TestSplitMCPToolName_PrefersLongestKnownServer(t *testing.T) {
	t.Parallel()

	server, tool, ok := proto.SplitMCPToolName("mcp_my_server_do_thing", []string{"my", "my_server"})
	require.True(t, ok)
	require.Equal(t, "my_server", server)
	require.Equal(t, "do_thing", tool)
}

// TestSplitMCPToolName_FallsBackWithoutKnownServers pins the degraded
// behavior for a caller with no config in hand (or a session recorded
// before a server was renamed/removed): it still splits at the first
// underscore, matching the codebase's historical behavior, rather than
// refusing to render anything.
func TestSplitMCPToolName_FallsBackWithoutKnownServers(t *testing.T) {
	t.Parallel()

	server, tool, ok := proto.SplitMCPToolName("mcp_context7_query-docs", nil)
	require.True(t, ok)
	require.Equal(t, "context7", server)
	require.Equal(t, "query-docs", tool)
}

// TestSplitMCPToolName_RejectsNonMCPName pins the failure mode for a name
// that isn't an MCP tool call at all.
func TestSplitMCPToolName_RejectsNonMCPName(t *testing.T) {
	t.Parallel()

	_, _, ok := proto.SplitMCPToolName("grep", nil)
	require.False(t, ok)
}
