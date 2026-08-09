package backend_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rave-soft/braid/internal/agent/tools/mcp"
	"github.com/rave-soft/braid/internal/backend"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/stretchr/testify/require"
)

// TestBackend_WorkspaceMCPIsolation verifies that MCP registry state is
// per-workspace, not process-global (ARCHITECTURE_REVIEW.md section 3.1,
// stage 3). Two workspaces in the same backend process each configure an
// MCP server under the SAME name; if the registries were still shared,
// whichever workspace initialized last would clobber the other's state
// instead of each keeping its own.
//
// The two configs are deliberately network-free: an OAuth HTTP server with
// no cached token transitions straight to StateNeedsAuth without dialing
// (see Registry.initClient), and a disabled server transitions to
// StateDisabled without even attempting a connection. This keeps the test
// hermetic while still exercising real Initialize/WaitForInit plumbing.
func TestBackend_WorkspaceMCPIsolation(t *testing.T) {
	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(hostHome, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(hostHome, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(hostHome, ".cache"))

	const sharedName = "shared-mcp"

	wdA := t.TempDir()
	wdB := t.TempDir()
	writeMCPConfig(t, wdA, `{"mcp": {"`+sharedName+`": {"type": "http", "url": "https://a.example.invalid/mcp", "oauth": true}}}`)
	writeMCPConfig(t, wdB, `{"mcp": {"`+sharedName+`": {"type": "http", "disabled": true}}}`)

	srvCfg, err := config.Init(wdA, "", false)
	require.NoError(t, err)
	b := backend.New(t.Context(), srvCfg, nil)

	cidA := uuid.New().String()
	cidB := uuid.New().String()

	wsA, _, err := b.CreateWorkspace(proto.Workspace{
		ClientID: cidA,
		Path:     wdA,
		DataDir:  filepath.Join(wdA, ".braid"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.DeleteWorkspace(wsA.ID, cidA) })

	wsB, _, err := b.CreateWorkspace(proto.Workspace{
		ClientID: cidB,
		Path:     wdB,
		DataDir:  filepath.Join(wdB, ".braid"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.DeleteWorkspace(wsB.ID, cidB) })

	require.NotNil(t, wsA.MCP, "workspace A must have its own MCP registry")
	require.NotNil(t, wsB.MCP, "workspace B must have its own MCP registry")
	require.NotSame(t, wsA.MCP, wsB.MCP, "MCP registries must be distinct instances per workspace")

	waitCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, wsA.MCP.WaitForInit(waitCtx))
	require.NoError(t, wsB.MCP.WaitForInit(waitCtx))

	stateA, ok := wsA.MCP.GetState(sharedName)
	require.True(t, ok, "workspace A must have state for its own %q server", sharedName)
	require.Equal(t, mcp.StateNeedsAuth, stateA.State,
		"workspace A's %q must be StateNeedsAuth from its own OAuth config, not overwritten by workspace B's", sharedName)

	stateB, ok := wsB.MCP.GetState(sharedName)
	require.True(t, ok, "workspace B must have state for its own %q server", sharedName)
	require.Equal(t, mcp.StateDisabled, stateB.State,
		"workspace B's %q must be StateDisabled from its own config, not overwritten by workspace A's", sharedName)

	// Same check through the backend's public API, which is what real
	// callers (server handlers, workspace.AppWorkspace) actually use.
	statesA := b.MCPGetStates(wsA.ID)
	statesB := b.MCPGetStates(wsB.ID)
	require.Equal(t, mcp.StateNeedsAuth, statesA[sharedName].State)
	require.Equal(t, mcp.StateDisabled, statesB[sharedName].State)
}

// TestBackend_WorkspaceLSPIsolation verifies that LSP client state is
// per-workspace, not process-global (ARCHITECTURE_REVIEW.md section 3.1,
// stage 3). Two workspaces in the same backend process each configure a
// differently-named LSP server; before this stage, both shared
// internal/app's package-level lspStates/lspBroker, so either workspace's
// GetLSPStates() would return every workspace's LSP clients.
func TestBackend_WorkspaceLSPIsolation(t *testing.T) {
	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(hostHome, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(hostHome, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(hostHome, ".cache"))

	// app.New only wires up the LSP callback (and hence TrackConfigured)
	// once the agent is configured, so each workspace needs a minimal
	// (unreachable) provider/model in addition to its LSP entry.
	const minimalAgentConfig = `,
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}`

	wdA := t.TempDir()
	wdB := t.TempDir()
	writeMCPConfig(t, wdA, `{"lsp": {"only-in-a": {"command": "nonexistent-a", "filetypes": ["go"]}}`+minimalAgentConfig+`}`)
	writeMCPConfig(t, wdB, `{"lsp": {"only-in-b": {"command": "nonexistent-b", "filetypes": ["go"]}}`+minimalAgentConfig+`}`)

	srvCfg, err := config.Init(wdA, "", false)
	require.NoError(t, err)
	b := backend.New(t.Context(), srvCfg, nil)

	cidA := uuid.New().String()
	cidB := uuid.New().String()

	wsA, _, err := b.CreateWorkspace(proto.Workspace{
		ClientID: cidA,
		Path:     wdA,
		DataDir:  filepath.Join(wdA, ".braid"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.DeleteWorkspace(wsA.ID, cidA) })

	wsB, _, err := b.CreateWorkspace(proto.Workspace{
		ClientID: cidB,
		Path:     wdB,
		DataDir:  filepath.Join(wdB, ".braid"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.DeleteWorkspace(wsB.ID, cidB) })

	require.NotNil(t, wsA.LSPManager, "workspace A must have its own LSP manager")
	require.NotNil(t, wsB.LSPManager, "workspace B must have its own LSP manager")
	require.NotSame(t, wsA.LSPManager, wsB.LSPManager, "LSP managers must be distinct instances per workspace")

	// TrackConfigured announces each workspace's configured-but-not-yet-started
	// LSPs asynchronously (see app.New); poll until each shows up.
	require.Eventually(t, func() bool {
		statesA, err := b.GetLSPStates(wsA.ID)
		return err == nil && len(statesA) > 0
	}, 2*time.Second, 10*time.Millisecond, "workspace A's LSP state never appeared")

	require.Eventually(t, func() bool {
		statesB, err := b.GetLSPStates(wsB.ID)
		return err == nil && len(statesB) > 0
	}, 2*time.Second, 10*time.Millisecond, "workspace B's LSP state never appeared")

	statesA, err := b.GetLSPStates(wsA.ID)
	require.NoError(t, err)
	statesB, err := b.GetLSPStates(wsB.ID)
	require.NoError(t, err)

	_, hasA := statesA["only-in-a"]
	_, leakedB := statesA["only-in-b"]
	require.True(t, hasA, "workspace A must see its own LSP client")
	require.False(t, leakedB, "workspace A must not see workspace B's LSP client")

	_, hasB := statesB["only-in-b"]
	_, leakedA := statesB["only-in-a"]
	require.True(t, hasB, "workspace B must see its own LSP client")
	require.False(t, leakedA, "workspace B must not see workspace A's LSP client")
}

func writeMCPConfig(t *testing.T, workingDir, jsonContent string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(workingDir, "braid.json"), []byte(jsonContent), 0o644))
}
