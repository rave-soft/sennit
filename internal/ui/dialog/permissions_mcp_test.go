package dialog

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// configOnlyWorkspace answers Config() from cfg and panics on every other
// FrontendWorkspace method — enough to drive renderToolName's config
// lookup without standing up a real workspace.
type configOnlyWorkspace struct {
	workspace.FrontendWorkspace
	cfg *config.Config
}

func (w configOnlyWorkspace) Config() *config.Config { return w.cfg }

// TestPermissions_MCPToolNameResolvesServerWithUnderscore pins Audit 12
// finding 5 for the permission dialog: the tool header used to split an
// MCP tool's composite name on the first two underscores, which for a
// server config key containing its own underscore ("my_server") produced
// "My → Server Do Thing" instead of "My Server → Do Thing". With the
// server's real name available from config, the header must resolve the
// true boundary.
func TestPermissions_MCPToolNameResolvesServerWithUnderscore(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	com := &common.Common{
		Styles: &sty,
		Workspace: configOnlyWorkspace{
			cfg: &config.Config{MCP: config.MCPs{"my_server": config.MCPConfig{}}},
		},
	}
	perm := permission.PermissionRequest{
		ID:         "perm-mcp",
		ToolCallID: "tool-call-mcp",
		ToolName:   "mcp_my_server_do_thing",
	}
	p := NewPermissions(com, perm)

	out := ansi.Strip(p.renderToolName(80))
	require.Contains(t, out, "My Server")
	require.Contains(t, out, "Do Thing")
	require.NotContains(t, out, "Server Do Thing",
		"the server's own underscore must not leak into the tool part")
}

// TestPermissions_MCPToolNameFallsBackWithoutConfig pins the guard added
// alongside the fix: renderToolName must not panic when Common has no
// Workspace wired up (the zero-value Common tests build directly), and
// must still render something for an MCP-shaped name.
func TestPermissions_MCPToolNameFallsBackWithoutConfig(t *testing.T) {
	t.Parallel()

	p := newTestPermissions(t)
	p.permission.ToolName = "mcp_context7_query-docs"

	require.NotPanics(t, func() {
		out := ansi.Strip(p.renderToolName(80))
		require.Contains(t, out, "Context7")
		require.Contains(t, out, "Query Docs")
	})
}
