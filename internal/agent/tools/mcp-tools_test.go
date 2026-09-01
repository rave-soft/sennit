package tools

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/stretchr/testify/require"
)

// stubMCPConfigProvider serves a fixed *config.Config to the whitelist check.
type stubMCPConfigProvider struct {
	cfg *config.Config
}

func (s *stubMCPConfigProvider) Config() *config.Config             { return s.cfg }
func (s *stubMCPConfigProvider) Resolver() config.VariableResolver  { return nil }
func (s *stubMCPConfigProvider) Overrides() config.RuntimeOverrides { return config.RuntimeOverrides{} }

func (s *stubMCPConfigProvider) ReserveMCPTokenMutation(string, config.MCPConfig) (config.MCPTokenMutation, bool) {
	return config.MCPTokenMutation{}, false
}

func (s *stubMCPConfigProvider) SetMCPTokenContext(context.Context, *config.MCPTokenMutation, *oauth.Token) (bool, error) {
	return false, nil
}

func (s *stubMCPConfigProvider) ClearMCPToken(*config.MCPTokenMutation, *oauth.Token) (bool, error) {
	return false, nil
}

var _ mcp.ConfigProvider = (*stubMCPConfigProvider)(nil)

// TestIsWhitelistedDockerTool_OnlyForTheManagedGateway is the regression
// test for the permission bypass keyed on a server *name*: any user server
// that happened to be called "docker" used to get its mcp-add/mcp-config-set
// calls through without a permission request.
func TestIsWhitelistedDockerTool_OnlyForTheManagedGateway(t *testing.T) {
	t.Parallel()

	managed := config.DockerMCPConfig()
	userServer := config.MCPConfig{Type: config.MCPStdio, Command: "/home/user/bin/evil", Args: []string{"serve"}}

	toolFor := func(server string, toolName string, mc config.MCPConfig) *Tool {
		return &Tool{
			mcpName: server,
			tool:    &mcp.Tool{Name: toolName},
			cfg:     &stubMCPConfigProvider{cfg: &config.Config{MCP: map[string]config.MCPConfig{server: mc}}},
		}
	}

	require.True(t, toolFor("docker", "mcp-find", managed).isWhitelistedDockerTool(),
		"the managed gateway keeps its whitelist")
	require.False(t, toolFor("docker", "mcp-add", userServer).isWhitelistedDockerTool(),
		"a user server merely named docker gets no bypass")
	require.False(t, toolFor("docker", "run-anything", managed).isWhitelistedDockerTool(),
		"a tool off the whitelist still needs permission")
	require.False(t, toolFor("other", "mcp-find", managed).isWhitelistedDockerTool(),
		"other servers never bypass")

	nilCfg := &Tool{mcpName: "docker", tool: &mcp.Tool{Name: "mcp-find"}, cfg: &stubMCPConfigProvider{}}
	require.False(t, nilCfg.isWhitelistedDockerTool(), "no config means fail closed")
}
