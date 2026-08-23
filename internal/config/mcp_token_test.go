package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestSetMCPToken_ProjectScopedServerPersistsToWorkspace covers a server
// declared only in a project config (no entry in the global data file).
// mutateMCPToken must fall back to ScopeWorkspace and write a token-only
// overlay there, rather than silently no-op'ing against the global file
// (the bug this test guards against: gjson.GetBytes(global, "mcp.<name>")
// never exists for a project-scoped server, so the old hardcoded
// s.atomicWrite(ScopeGlobal, ...) always returned errAtomicWriteNoop).
func TestSetMCPToken_ProjectScopedServerPersistsToWorkspace(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", dataDir)

	workingDir := t.TempDir()
	projectSeed := `{"mcp":{"server":{"type":"http","url":"https://example.test","oauth":true}}}`
	require.NoError(t, os.WriteFile(filepath.Join(workingDir, "sennit.json"), []byte(projectSeed), 0o644))

	store, err := Load(workingDir, "", false)
	require.NoError(t, err)

	mcp, ok := store.Config().MCP["server"]
	require.True(t, ok)
	require.Nil(t, mcp.OAuthToken)

	// The global data file must not declare the server: that is the
	// precondition this bug depends on. It may not even exist yet.
	require.False(t, gjson.GetBytes(readFileOrEmpty(t, store.globalDataPath), "mcp.server").Exists())

	reservation, ok := store.ReserveMCPTokenMutation("server", mcp)
	require.True(t, ok)

	token := &oauth.Token{AccessToken: "project-token"}
	changed, err := store.SetMCPToken(&reservation, token)
	require.NoError(t, err)
	require.True(t, changed)

	// Persisted to the workspace config, as a token-only overlay, and NOT
	// to the global data file.
	workspaceData := requireFile(t, store.workspacePath)
	require.Equal(t, "project-token", gjson.GetBytes(workspaceData, "mcp.server.oauth_token.access_token").String())
	// No bogus full declaration: the overlay carries only the token.
	require.False(t, gjson.GetBytes(workspaceData, "mcp.server.type").Exists())
	require.False(t, gjson.GetBytes(workspaceData, "mcp.server.url").Exists())

	require.False(t, gjson.GetBytes(readFileOrEmpty(t, store.globalDataPath), "mcp.server").Exists())

	// In-memory config reflects the token immediately.
	require.Equal(t, "project-token", store.Config().MCP["server"].OAuthToken.AccessToken)

	// The token survives a reload: the workspace overlay merges back onto
	// the project's full declaration, so both the token and the original
	// command/url survive.
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	reloaded, ok := store.Config().MCP["server"]
	require.True(t, ok)
	require.Equal(t, "project-token", reloaded.OAuthToken.AccessToken)
	require.Equal(t, MCPHttp, reloaded.Type)
	require.Equal(t, "https://example.test", reloaded.URL)
}

// readFileOrEmpty reads path, treating a missing file as empty content: the
// global data file is only created on first write, so tests asserting on
// its absence of a key must tolerate it not existing yet.
func readFileOrEmpty(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	return data
}

// TestSetMCPToken_GloballyDeclaredServerStillWritesGlobal is a control for
// the fallback above: when the server is declared in the global data file,
// behaviour is unchanged from before this fix — the token is written there,
// not to the workspace config.
func TestSetMCPToken_GloballyDeclaredServerStillWritesGlobal(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", dataDir)

	workingDir := t.TempDir()
	globalSeed := `{"mcp":{"server":{"type":"http","url":"https://example.test","oauth":true}}}`
	dataConfigPath := GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(dataConfigPath), 0o755))
	require.NoError(t, os.WriteFile(dataConfigPath, []byte(globalSeed), 0o644))

	store, err := Load(workingDir, "", false)
	require.NoError(t, err)

	mcp, ok := store.Config().MCP["server"]
	require.True(t, ok)

	reservation, ok := store.ReserveMCPTokenMutation("server", mcp)
	require.True(t, ok)
	token := &oauth.Token{AccessToken: "global-token"}
	changed, err := store.SetMCPToken(&reservation, token)
	require.NoError(t, err)
	require.True(t, changed)

	require.Equal(t, "global-token", gjson.GetBytes(requireFile(t, store.globalDataPath), "mcp.server.oauth_token.access_token").String())
	// No workspace file should have been created for this write.
	_, statErr := os.Stat(store.workspacePath)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

// TestApplyWorkspaceConfig_TokenOnlyOverlayMergesWithoutBogusServer verifies
// the merge property the fix above depends on: a workspace config holding
// nothing but mcp.<name>.oauth_token deep-merges onto a full declaration
// that already exists in cfg (as loaded from a project config), and does
// not materialise any bogus server (one with no command/url) elsewhere.
func TestApplyWorkspaceConfig_TokenOnlyOverlayMergesWithoutBogusServer(t *testing.T) {
	workingDir := t.TempDir()
	workspaceDir := filepath.Join(t.TempDir(), "workspace-data")
	workspacePath := filepath.Join(workspaceDir, appName+".json")
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))

	cfg := &Config{
		MCP: MCPs{
			"server": {Type: MCPHttp, URL: "https://example.test", OAuth: true},
		},
	}
	cfg.setDefaults(workingDir, workspaceDir)

	overlay := `{"mcp":{"server":{"oauth_token":{"access_token":"overlay-token"}}}}`
	require.NoError(t, os.WriteFile(workspacePath, []byte(overlay), 0o644))

	var loaded []string
	require.NoError(t, applyWorkspaceConfig(cfg, workingDir, &loaded))

	require.Len(t, cfg.MCP, 1, "the token-only overlay must not materialise a second, bogus server entry")
	merged, ok := cfg.MCP["server"]
	require.True(t, ok)
	require.Equal(t, MCPHttp, merged.Type, "the project declaration's command/url fields must survive the merge")
	require.Equal(t, "https://example.test", merged.URL)
	require.NotNil(t, merged.OAuthToken)
	require.Equal(t, "overlay-token", merged.OAuthToken.AccessToken)
}
