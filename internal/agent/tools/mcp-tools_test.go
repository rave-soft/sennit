package tools

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
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

// cancelingRunner stands in for the real *mcp.Registry, always failing the
// way an in-flight MCP call does when the caller's own context is
// cancelled (Esc on a queued tool call) — see connectionManager's
// getOrRenewClient in mcp/connection.go, which returns ctx.Err() directly
// for exactly this case.
type cancelingRunner struct{ err error }

func (c cancelingRunner) RunTool(context.Context, mcp.ConfigProvider, string, string, string) (mcp.ToolResult, error) {
	return mcp.ToolResult{}, c.err
}

// TestToolRun_PropagatesCancellationAsGoError is the regression test for
// the "context canceled" text result: RunTool failing with
// context.Canceled used to come back as an ordinary (if IsError) tool
// response, so the model saw a normal-looking failure it could retry
// instead of the batch aborting. See AGENTS.md's "Tool failures: text
// response vs. Go error".
func TestToolRun_PropagatesCancellationAsGoError(t *testing.T) {
	t.Parallel()

	tool := &Tool{
		mcpName: "srv",
		tool:    &mcp.Tool{Name: "do_thing"},
		cfg:     &stubMCPConfigProvider{cfg: &config.Config{}},
		reg:     cancelingRunner{err: context.Canceled},
	}
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "sess")

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "1", Name: tool.Name(), Input: "{}"})
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled))
	require.Equal(t, fantasy.ToolResponse{}, resp)
}

// TestToolRun_ExpiredContextPropagatesAsGoError covers the ctx.Err() half
// of the check: even an error that does not itself wrap
// context.DeadlineExceeded must still abort the batch once the caller's
// own context has expired.
func TestToolRun_ExpiredContextPropagatesAsGoError(t *testing.T) {
	t.Parallel()

	tool := &Tool{
		mcpName: "srv",
		tool:    &mcp.Tool{Name: "do_thing"},
		cfg:     &stubMCPConfigProvider{cfg: &config.Config{}},
		reg:     cancelingRunner{err: errors.New("mcp: connection reset")},
	}
	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), SessionIDContextKey, "sess"), 0)
	defer cancel()
	<-ctx.Done()

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "1", Name: tool.Name(), Input: "{}"})
	require.Error(t, err)
	require.Equal(t, fantasy.ToolResponse{}, resp)
}

// TestToolRun_LostOwnershipStaysTextResponse is the regression test for
// G22: mcp.ErrLostOwnership signals that this attempt merely lost a race
// against a concurrent reconnect/teardown/auth flow (e.g. a lazy renewal
// of a dropped stdio server overlapping a config edit), not that the
// caller's own ctx was cancelled. ctx.Err() is nil here, exactly like the
// production case - the batch-aborting branch above must not trigger just
// because errLostOwnership used to be indistinguishable from
// context.Canceled before this fix.
func TestToolRun_LostOwnershipStaysTextResponse(t *testing.T) {
	t.Parallel()

	tool := &Tool{
		mcpName: "srv",
		tool:    &mcp.Tool{Name: "do_thing"},
		cfg:     &stubMCPConfigProvider{cfg: &config.Config{}},
		reg:     cancelingRunner{err: mcp.ErrLostOwnership},
	}
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "sess")
	require.NoError(t, ctx.Err())

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "1", Name: tool.Name(), Input: "{}"})
	require.NoError(t, err, "lost ownership must not abort the tool-call batch")
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "lost ownership")
}

// TestToolRun_GenuineToolFailureStaysTextResponse pins the other side of
// the split: an ordinary MCP failure (not cancellation) must still come
// back as a text response the model can react to.
func TestToolRun_GenuineToolFailureStaysTextResponse(t *testing.T) {
	t.Parallel()

	tool := &Tool{
		mcpName: "srv",
		tool:    &mcp.Tool{Name: "do_thing"},
		cfg:     &stubMCPConfigProvider{cfg: &config.Config{}},
		reg:     cancelingRunner{err: errors.New("mcp: no such tool")},
	}
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "sess")

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "1", Name: tool.Name(), Input: "{}"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "no such tool")
}
