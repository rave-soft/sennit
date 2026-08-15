package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/oauth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestMCPTokenMutationIsConditionalAndOwnerOrdered(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "braid.json")
	mcp := MCPConfig{Type: MCPHttp, URL: "https://example.test", OAuth: true, OAuthToken: &oauth.Token{AccessToken: "initial"}}
	require.NoError(t, os.WriteFile(path, []byte(`{"mcp":{"server":{"type":"http","url":"https://example.test","oauth":true,"oauth_token":{"access_token":"initial"}}}}`), 0o600))
	store := NewTestStore(&Config{MCP: MCPs{"server": mcp}})
	store.globalDataPath = path

	old, ok := store.ReserveMCPTokenMutation("server", mcp)
	require.True(t, ok)
	fresh, ok := store.ReserveMCPTokenMutation("server", mcp)
	require.True(t, ok)
	changed, err := store.SetMCPToken(&old, &oauth.Token{AccessToken: "stale"})
	require.NoError(t, err)
	require.False(t, changed)
	changed, err = store.SetMCPToken(&fresh, &oauth.Token{AccessToken: "fresh"})
	require.NoError(t, err)
	require.True(t, changed)
	changed, err = store.ClearMCPToken(&old, mcp.OAuthToken)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, "fresh", gjson.GetBytes(requireFile(t, path), "mcp.server.oauth_token.access_token").String())

	reservation, ok := store.ReserveMCPTokenMutation("server", store.Config().MCP["server"])
	require.True(t, ok)
	require.NoError(t, os.WriteFile(path, []byte(`{"mcp":{}}`), 0o600))
	changed, err = store.SetMCPToken(&reservation, &oauth.Token{AccessToken: "resurrected"})
	require.NoError(t, err)
	require.False(t, changed)
	require.False(t, gjson.GetBytes(requireFile(t, path), "mcp.server").Exists())

	deleted, ok := store.ReserveMCPTokenMutation("server", store.Config().MCP["server"])
	require.True(t, ok)
	require.NoError(t, os.Remove(path))
	changed, err = store.SetMCPToken(&deleted, &oauth.Token{AccessToken: "resurrected"})
	require.NoError(t, err)
	require.False(t, changed)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestMCPTokenMutationRejectsStaleStore(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "braid.json")
	initial := &oauth.Token{AccessToken: "initial"}
	mcp := MCPConfig{Type: MCPHttp, URL: "https://example.test", OAuth: true, OAuthToken: initial}
	require.NoError(t, os.WriteFile(path, []byte(`{"mcp":{"server":{"type":"http","url":"https://example.test","oauth":true,"oauth_token":{"access_token":"initial"}}}}`), 0o600))

	staleStore := NewTestStore(&Config{MCP: MCPs{"server": mcp}})
	staleStore.globalDataPath = path
	freshStore := NewTestStore(&Config{MCP: MCPs{"server": mcp}})
	freshStore.globalDataPath = path
	stale, ok := staleStore.ReserveMCPTokenMutation("server", mcp)
	require.True(t, ok)
	fresh, ok := freshStore.ReserveMCPTokenMutation("server", mcp)
	require.True(t, ok)

	changed, err := freshStore.SetMCPToken(&fresh, &oauth.Token{AccessToken: "fresh"})
	require.NoError(t, err)
	require.True(t, changed)
	changed, err = staleStore.SetMCPToken(&stale, &oauth.Token{AccessToken: "stale"})
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, "fresh", gjson.GetBytes(requireFile(t, path), "mcp.server.oauth_token.access_token").String())

	changed, err = staleStore.ClearMCPToken(&stale, initial)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, "fresh", gjson.GetBytes(requireFile(t, path), "mcp.server.oauth_token.access_token").String())
}

func requireFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func TestConfigStore_ConfigPath_GlobalAlwaysWorks(t *testing.T) {
	t.Parallel()

	store := &ConfigStore{
		globalDataPath: "/some/global/braid.json",
	}

	path, err := store.configPath(ScopeGlobal)
	require.NoError(t, err)
	require.Equal(t, "/some/global/braid.json", path)
}

func TestConfigStore_ConfigPath_WorkspaceReturnsPath(t *testing.T) {
	t.Parallel()

	store := &ConfigStore{
		workspacePath: "/some/workspace/.braid/braid.json",
	}

	path, err := store.configPath(ScopeWorkspace)
	require.NoError(t, err)
	require.Equal(t, "/some/workspace/.braid/braid.json", path)
}

func TestConfigStore_ConfigPath_WorkspaceErrorsWhenEmpty(t *testing.T) {
	t.Parallel()

	store := &ConfigStore{
		globalDataPath: "/some/global/braid.json",
		workspacePath:  "",
	}

	_, err := store.configPath(ScopeWorkspace)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoWorkspaceConfig))
}

func TestConfigStore_SetConfigField_WorkspaceScopeGuard(t *testing.T) {
	t.Parallel()

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: filepath.Join(t.TempDir(), "global.json"),
		workspacePath:  "",
	}

	err := store.SetConfigField(ScopeWorkspace, "foo", "bar")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoWorkspaceConfig))
}

func TestConfigStore_SetConfigField_GlobalScopeAlwaysWorks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	globalPath := filepath.Join(dir, "braid.json")
	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: globalPath,
	}

	err := store.SetConfigField(ScopeGlobal, "foo", "bar")
	require.NoError(t, err)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	require.Contains(t, string(data), `"foo"`)
}

func TestConfigStore_RemoveConfigField_WorkspaceScopeGuard(t *testing.T) {
	t.Parallel()

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: filepath.Join(t.TempDir(), "global.json"),
		workspacePath:  "",
	}

	err := store.RemoveConfigField(ScopeWorkspace, "foo")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoWorkspaceConfig))
}

func TestConfigStore_HasConfigField_WorkspaceScopeGuard(t *testing.T) {
	t.Parallel()

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: filepath.Join(t.TempDir(), "global.json"),
		workspacePath:  "",
	}

	has := store.HasConfigField(ScopeWorkspace, "foo")
	require.False(t, has)
}

func TestConfigStore_RuntimeOverrides_Independent(t *testing.T) {
	t.Parallel()

	store1 := &ConfigStore{config: &Config{}}
	store2 := &ConfigStore{config: &Config{}}

	require.False(t, store1.Overrides().SkipPermissionRequests)
	require.False(t, store2.Overrides().SkipPermissionRequests)

	store1.Overrides().SkipPermissionRequests = true

	require.True(t, store1.Overrides().SkipPermissionRequests)
	require.False(t, store2.Overrides().SkipPermissionRequests)
}

func TestConfigStore_RuntimeOverrides_MutableViaPointer(t *testing.T) {
	t.Parallel()

	store := &ConfigStore{config: &Config{}}
	overrides := store.Overrides()

	require.False(t, overrides.SkipPermissionRequests)

	overrides.SkipPermissionRequests = true
	require.True(t, store.Overrides().SkipPermissionRequests)
}

func TestGlobalWorkspaceDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRAID_GLOBAL_DATA", dir)

	wsDir := GlobalWorkspaceDir()
	globalData := GlobalConfigData()

	require.Equal(t, filepath.Dir(globalData), wsDir)
	require.Equal(t, dir, wsDir)
}

func TestScope_String(t *testing.T) {
	t.Parallel()

	require.Equal(t, "global", ScopeGlobal.String())
	require.Equal(t, "workspace", ScopeWorkspace.String())
	require.Contains(t, Scope(99).String(), "Scope(99)")
}

func TestConfigStaleness_CleanImmediatelyAfterSnapshot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Create a config file
	content := []byte(`{"options": {"debug": true}}`)
	require.NoError(t, os.WriteFile(configPath, content, 0o600))

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}
	store.captureStalenessSnapshot([]string{configPath})

	result := store.ConfigStaleness()
	require.False(t, result.Dirty)
	require.Empty(t, result.Changed)
	require.Empty(t, result.Missing)
}

func TestConfigStaleness_DetectsFileContentChange(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Create initial config file
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": false}`), 0o600))

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}
	store.captureStalenessSnapshot([]string{configPath})

	// Modify the file
	time.Sleep(10 * time.Millisecond) // Ensure different mtime
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": true}`), 0o600))

	result := store.ConfigStaleness()
	require.True(t, result.Dirty)
	require.Contains(t, result.Changed, configPath)
	require.Empty(t, result.Missing)
}

func TestConfigStaleness_DetectsFileDeletion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Create initial config file
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": true}`), 0o600))

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}
	store.captureStalenessSnapshot([]string{configPath})

	// Delete the file
	require.NoError(t, os.Remove(configPath))

	result := store.ConfigStaleness()
	require.True(t, result.Dirty)
	require.Empty(t, result.Changed)
	require.Contains(t, result.Missing, configPath)
}

func TestConfigStaleness_DetectsNewFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Don't create file initially
	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}
	store.captureStalenessSnapshot([]string{configPath})

	// Now create the file
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": true}`), 0o600))

	result := store.ConfigStaleness()
	require.True(t, result.Dirty)
	require.Contains(t, result.Changed, configPath)
	require.Empty(t, result.Missing)
}

func TestConfigStaleness_SortedOutput(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.json")
	pathB := filepath.Join(dir, "b.json")
	pathC := filepath.Join(dir, "c.json")

	// Create all files
	for _, p := range []string{pathA, pathB, pathC} {
		require.NoError(t, os.WriteFile(p, []byte(`{}`), 0o600))
	}

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: pathA,
	}
	// Add in reverse order to test sorting
	store.captureStalenessSnapshot([]string{pathC, pathA, pathB})

	// Modify all files
	time.Sleep(10 * time.Millisecond)
	for _, p := range []string{pathA, pathB, pathC} {
		require.NoError(t, os.WriteFile(p, []byte(`{"changed": true}`), 0o600))
	}

	result := store.ConfigStaleness()
	require.True(t, result.Dirty)
	// Should be sorted alphabetically
	require.Equal(t, []string{pathA, pathB, pathC}, result.Changed)
}

func TestConfigStaleness_RefreshClearsDirtyState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Create initial config file
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": false}`), 0o600))

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}
	store.captureStalenessSnapshot([]string{configPath})

	// Modify the file
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": true}`), 0o600))

	// Verify dirty
	result := store.ConfigStaleness()
	require.True(t, result.Dirty)

	// Refresh snapshot
	require.NoError(t, store.RefreshStalenessSnapshot())

	// Verify clean now
	result = store.ConfigStaleness()
	require.False(t, result.Dirty)
	require.Empty(t, result.Changed)
	require.Empty(t, result.Missing)
}

func TestReloadFromDisk_WorkspaceMergeErrorKeepsPublishedConfig(t *testing.T) {
	workingDir := t.TempDir()
	globalDir := t.TempDir()
	workspaceDir := filepath.Join(t.TempDir(), "custom-workspace-data")
	t.Setenv("BRAID_GLOBAL_CONFIG", globalDir)
	t.Setenv("BRAID_GLOBAL_DATA", globalDir)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, appName+".json"), []byte(`{"options":{"data_directory":"`+workspaceDir+`"}}`), 0o644))

	workspacePath := filepath.Join(workspaceDir, appName+".json")
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
	require.NoError(t, os.WriteFile(workspacePath, []byte(`{"options":{"debug":true}}`), 0o644))

	store, err := Load(workingDir, "", false)
	require.NoError(t, err)
	published := store.Config()
	require.True(t, published.Options.Debug)
	require.Equal(t, workspacePath, store.workspacePath)

	require.NoError(t, os.WriteFile(workspacePath, []byte(`{"options":"invalid"}`), 0o644))
	var logs strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	require.NoError(t, store.ReloadFromDisk(context.Background()))
	require.True(t, published.Options.Debug)
	require.False(t, store.Config().Options.Debug)
	require.NotContains(t, store.LoadedPaths(), workspacePath)
	require.Equal(t, workspacePath, store.workspacePath)
	require.Contains(t, logs.String(), workspacePath)
	require.Contains(t, logs.String(), "type mismatch")
}

func TestReloadFromDisk_WorkspaceLegacyRecentModelsPreservesSiblingFields(t *testing.T) {
	workingDir := t.TempDir()
	globalDir := t.TempDir()
	workspaceDir := filepath.Join(t.TempDir(), "custom-workspace-data")
	t.Setenv("BRAID_GLOBAL_CONFIG", globalDir)
	t.Setenv("BRAID_GLOBAL_DATA", globalDir)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, appName+".json"), []byte(`{"options":{"data_directory":"`+workspaceDir+`"}}`), 0o644))

	workspacePath := filepath.Join(workspaceDir, appName+".json")
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
	require.NoError(t, os.WriteFile(workspacePath, []byte(`{"options":{"debug":false}}`), 0o644))

	store, err := Load(workingDir, "", false)
	require.NoError(t, err)
	require.False(t, store.Config().Options.Debug)

	require.NoError(t, os.WriteFile(workspacePath, []byte(`{"recent_models":{"small":[]},"options":{"debug":true}}`), 0o644))
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	require.True(t, store.Config().Options.Debug)
	require.Empty(t, store.Config().RecentModels)
	require.Equal(t, workspaceDir, store.Config().Options.DataDirectory)
	require.Contains(t, store.LoadedPaths(), workspacePath)
}

// TestReloadFromDisk_UsesNewConfigValues is a regression test ensuring that
// ReloadFromDisk updates store state BEFORE running model/agent setup,
// so the new config values are used rather than stale pre-reload values.
func TestReloadFromDisk_UsesNewConfigValues(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Isolate from the host's global config so only test-provided
	// providers are visible.
	t.Setenv("BRAID_GLOBAL_CONFIG", dir)
	t.Setenv("BRAID_GLOBAL_DATA", dir)

	// Create initial config with one model preference
	initialConfig := `{
		"model": {"provider": "openai", "model": "gpt-4"},
		"providers": {
			"openai": {
				"api_key": "test-key",
				"models": [{"id": "gpt-4", "name": "GPT-4"}]
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load initial config properly
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Set globalDataPath for the test (Load doesn't set this directly)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Verify initial model
	require.Equal(t, "openai", store.config.Model.Provider)
	require.Equal(t, "gpt-4", store.config.Model.Model)

	// Modify config on disk to change model
	updatedConfig := `{
		"model": {"provider": "anthropic", "model": "claude-3"},
		"providers": {
			"openai": {
				"api_key": "test-key",
				"models": [{"id": "gpt-4", "name": "GPT-4"}]
			},
			"anthropic": {
				"api_key": "test-key-2",
				"models": [{"id": "claude-3", "name": "Claude 3"}]
			}
		}
	}`
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(updatedConfig), 0o600))

	// Reload from disk
	ctx := context.Background()
	err = store.ReloadFromDisk(ctx)
	require.NoError(t, err)

	// Verify the NEW config values are now in effect (regression check)
	require.Equal(t, "anthropic", store.config.Model.Provider)
	require.Equal(t, "claude-3", store.config.Model.Model)
}

// TestSetConfigField_AutoReloads verifies that SetConfigField automatically
// reloads config into memory after writing, so subsequent reads see the new value.
func TestSetConfigField_AutoReloads(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Isolate from the host's real global config/data so this test only
	// ever sees the config it writes itself.
	t.Setenv("BRAID_GLOBAL_CONFIG", dir)
	t.Setenv("BRAID_GLOBAL_DATA", dir)

	// Create initial config file with debug = false
	initialConfig := `{"options": {"debug": false}}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load initial config
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Verify initial state
	require.False(t, store.config.Options.Debug)

	// Set globalDataPath and capture snapshot for staleness tracking
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Use SetConfigField to change debug to true
	err = store.SetConfigField(ScopeGlobal, "options.debug", true)
	require.NoError(t, err)

	// Verify in-memory state was automatically reloaded and reflects the change
	require.True(t, store.config.Options.Debug, "Expected config to auto-reload and show debug = true")

	// Verify staleness is clean after the reload
	staleness := store.ConfigStaleness()
	require.False(t, staleness.Dirty, "Expected staleness to be clean after auto-reload")
}

// TestRemoveConfigField_AutoReloads verifies that RemoveConfigField automatically
// reloads config into memory after writing.
func TestRemoveConfigField_AutoReloads(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Isolate from the host's real global config/data so this test only
	// ever sees the config it writes itself.
	t.Setenv("BRAID_GLOBAL_CONFIG", dir)
	t.Setenv("BRAID_GLOBAL_DATA", dir)

	// Create initial config file with a custom option
	initialConfig := `{"options": {"debug": true, "custom_field": "value"}}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load initial config
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Set globalDataPath and capture snapshot
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Verify the field exists initially (indirectly - store loaded successfully)
	require.True(t, store.config.Options.Debug)

	// Remove the debug field
	err = store.RemoveConfigField(ScopeGlobal, "options.debug")
	require.NoError(t, err)

	// Verify auto-reload occurred and stale state is clean
	staleness := store.ConfigStaleness()
	require.False(t, staleness.Dirty, "Expected staleness to be clean after auto-reload from RemoveConfigField")
}

// TestSetConfigField_AutoReloadSkipsWhenNoWorkingDir verifies that auto-reload
// gracefully skips when working directory is not set (e.g., during testing).
func TestSetConfigField_AutoReloadSkipsWhenNoWorkingDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Create a store without working directory (like some test setups)
	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
		// workingDir is empty
	}

	// SetConfigField should succeed even without workingDir (auto-reload skips)
	err := store.SetConfigField(ScopeGlobal, "foo", "bar")
	require.NoError(t, err)

	// Verify file was still written
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "foo")
}

// TestAutoReloadDisabledDuringReload verifies that auto-reload is suppressed
// during ReloadFromDisk to prevent re-entrant/nested reload calls.
func TestAutoReloadDisabledDuringReload(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Isolate from the host's real global config/data so this test only
	// ever sees the config it writes itself.
	t.Setenv("BRAID_GLOBAL_CONFIG", dir)
	t.Setenv("BRAID_GLOBAL_DATA", dir)

	// Create initial config with a provider that will trigger config modification during reload
	// (simulating the anthropic OAuth token removal case)
	initialConfig := `{
		"providers": {
			"anthropic": {
				"api_key": "test-key",
				"oauth": {"access_token": "token", "refresh_token": "refresh"}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load will trigger configureProviders which removes anthropic OAuth config.
	// This should NOT cause infinite recursion — writeMu prevents re-entrant reloads.
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Capture snapshot and verify reload also works without recursion
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Modify file and reload — this should work without re-entrancy issues
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(`{"options": {"debug": true}}`), 0o600))

	err = store.ReloadFromDisk(context.Background())
	require.NoError(t, err)
}

// TestSetConfigFields_AutoReloadsAtomically verifies that SetConfigFields writes
// multiple fields in a single disk write and triggers only one auto-reload,
// avoiding intermediate states where only some fields are persisted.
func TestSetConfigFields_AutoReloadsAtomically(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Isolate from the host's real global config/data so this test only
	// ever sees the config it writes itself.
	t.Setenv("BRAID_GLOBAL_CONFIG", dir)
	t.Setenv("BRAID_GLOBAL_DATA", dir)

	// Create initial config file.
	initialConfig := `{"options": {"debug": false}}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load initial config.
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Set globalDataPath and capture snapshot.
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Write multiple fields atomically.
	err = store.SetConfigFields(ScopeGlobal, map[string]any{
		"options.debug":  true,
		"options.custom": "hello",
	})
	require.NoError(t, err)

	// Verify both fields are reflected in memory.
	require.True(t, store.config.Options.Debug)
}

func TestLoadTokenFromDisk_ReturnsNewerToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Create config file with a newer token on disk
	configContent := `{
		"providers": {
			"hyper": {
				"oauth": {
					"access_token": "newer-token-from-disk",
					"refresh_token": "refresh-abc",
					"expires_in": 3600,
					"expires_at": 9999999999
				}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}

	token, err := store.loadTokenFromDisk(ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.NotNil(t, token)
	require.Equal(t, "newer-token-from-disk", token.AccessToken)
	require.Equal(t, "refresh-abc", token.RefreshToken)
	require.Equal(t, 3600, token.ExpiresIn)
	require.Equal(t, int64(9999999999), token.ExpiresAt)
}

func TestLoadTokenFromDisk_ReturnsNilWhenSameToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Create config file with the same token
	configContent := `{
		"providers": {
			"hyper": {
				"oauth": {
					"access_token": "same-token",
					"refresh_token": "refresh-abc",
					"expires_in": 3600,
					"expires_at": 9999999999
				}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}

	token, err := store.loadTokenFromDisk(ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.NotNil(t, token)
	require.Equal(t, "same-token", token.AccessToken)
}

func TestLoadTokenFromDisk_ReturnsNilWhenFileMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "nonexistent.json")

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}

	token, err := store.loadTokenFromDisk(ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.Nil(t, token)
}

func TestLoadTokenFromDisk_ReturnsNilWhenProviderMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Create config file without the hyper provider
	configContent := `{"providers": {"openai": {"api_key": "test-key"}}}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}

	token, err := store.loadTokenFromDisk(ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.Nil(t, token)
}

func TestLoadTokenFromDisk_ReturnsNilWhenOAuthMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Create config file with provider but no OAuth token
	configContent := `{"providers": {"hyper": {"api_key": "test-key"}}}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}

	token, err := store.loadTokenFromDisk(ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.Nil(t, token)
}

func TestRefreshOAuthToken_UsesDiskTokenWhenDifferent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Create config file with a newer token on disk
	configContent := `{
		"providers": {
			"hyper": {
				"api_key": "newer-access-token",
				"oauth": {
					"access_token": "newer-access-token",
					"refresh_token": "refresh-abc",
					"expires_in": 3600,
					"expires_at": 9999999999
				}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	// Set up store with an older in-memory token
	oldToken := &oauth.Token{
		AccessToken:  "older-access-token",
		RefreshToken: "refresh-abc",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(), // Expired
	}

	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("hyper", ProviderConfig{
		ID:         "hyper",
		Name:       "Hyper",
		APIKey:     oldToken.AccessToken,
		OAuthToken: oldToken,
	})

	store := &ConfigStore{
		config: &Config{
			Providers: providers,
		},
		globalDataPath: configPath,
	}

	// Refresh should use the disk token without making an external call
	err := store.RefreshOAuthToken(context.Background(), ScopeGlobal, "hyper")
	require.NoError(t, err)

	// Verify the in-memory token was updated to the disk token
	updatedConfig, ok := store.config.Providers.Get("hyper")
	require.True(t, ok)
	require.Equal(t, "newer-access-token", updatedConfig.APIKey)
	require.Equal(t, "newer-access-token", updatedConfig.OAuthToken.AccessToken)
	require.Equal(t, "refresh-abc", updatedConfig.OAuthToken.RefreshToken)
}

// TestConfigStore_SetConfigFields_concurrentInProcess verifies that
// concurrent in-process writes do not lose data when serialized by the
// s.mu mutex. This does not exercise the cross-process flock; testing
// that would require spawning a separate OS process.
func TestConfigStore_SetConfigFields_concurrentInProcess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o755))
	require.NoError(t, os.WriteFile(configPath, []byte("{}"), 0o600))

	store := &ConfigStore{
		config: &Config{
			Providers: csync.NewMap[string, ProviderConfig](),
		},
		globalDataPath: configPath,
		workingDir:     dir,
	}

	const (
		numGoroutines    = 20
		fieldsPerRoutine = 5
	)

	errs := make(chan error, numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			kv := make(map[string]any, fieldsPerRoutine)
			for j := 0; j < fieldsPerRoutine; j++ {
				key := fmt.Sprintf("goroutine_%d_field_%d", id, j)
				kv[key] = fmt.Sprintf("value_%d_%d", id, j)
			}
			errs <- store.SetConfigFields(ScopeGlobal, kv)
		}(i)
	}

	for i := 0; i < numGoroutines; i++ {
		require.NoError(t, <-errs)
	}

	// Verify all fields are present in the config file.
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)

	for i := 0; i < numGoroutines; i++ {
		for j := 0; j < fieldsPerRoutine; j++ {
			key := fmt.Sprintf("goroutine_%d_field_%d", i, j)
			expectedValue := fmt.Sprintf("value_%d_%d", i, j)
			result := gjson.Get(string(data), key)
			require.True(t, result.Exists(), "key %s should exist", key)
			require.Equal(t, expectedValue, result.String(), "key %s should have the correct value", key)
		}
	}
}

// TestSetProviderAPIKey_CustomOAuthProviderSurvivesReload covers fix 2.1
// (see ARCHITECTURE_REVIEW.md §3.4): writing an OAuth token for a provider
// outside the embedded catalog must also persist its type/base_url/name,
// or a later reload (which rebuilds Config from disk via
// configureProviders) drops the provider entirely for lacking a base_url.
func TestSetProviderAPIKey_CustomOAuthProviderSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Isolate from the host's real global config/data so only the
	// test-provided provider and the embedded catalog are visible.
	t.Setenv("BRAID_GLOBAL_CONFIG", dir)
	t.Setenv("BRAID_GLOBAL_DATA", dir)

	initialConfig := `{
		"providers": {
			"my-custom-oauth": {
				"type": "openai-compat",
				"base_url": "https://example.com/v1",
				"name": "My Custom OAuth",
				"models": [{"id": "custom-model", "name": "Custom Model"}]
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	_, exists := store.config.Providers.Get("my-custom-oauth")
	require.True(t, exists, "custom provider should have loaded from the initial config")

	token := &oauth.Token{
		AccessToken:  "custom-at",
		RefreshToken: "custom-rt",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}
	require.NoError(t, store.SetProviderAPIKey(ScopeGlobal, "my-custom-oauth", token))

	// The identity fields must have landed on disk alongside the credential.
	raw, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, "https://example.com/v1", gjson.GetBytes(raw, "providers.my-custom-oauth.base_url").String())
	require.Equal(t, "openai-compat", gjson.GetBytes(raw, "providers.my-custom-oauth.type").String())
	require.Equal(t, "My Custom OAuth", gjson.GetBytes(raw, "providers.my-custom-oauth.name").String())
	require.Equal(t, "custom-at", gjson.GetBytes(raw, "providers.my-custom-oauth.api_key").String())

	// This is the bug from §3.4: without the identity fields on disk, a
	// reload rebuilds Config from disk and drops the provider entirely.
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	pc, exists := store.config.Providers.Get("my-custom-oauth")
	require.True(t, exists, "custom OAuth provider must survive a reload")
	require.Equal(t, "https://example.com/v1", pc.BaseURL)
	require.Equal(t, "custom-at", pc.APIKey)
	require.NotNil(t, pc.OAuthToken)
	require.Equal(t, "custom-at", pc.OAuthToken.AccessToken)
}

// TestSetProviderAPIKey_CatalogProviderOmitsBaseURL covers the other half
// of fix 2.1: a provider that IS in the embedded catalog (Copilot) must not
// get its base_url pinned into the user's config file when a token is
// written. Doing so would freeze that provider against future catalog
// updates (see isCatalogProvider).
func TestSetProviderAPIKey_CatalogProviderOmitsBaseURL(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	t.Setenv("BRAID_GLOBAL_CONFIG", dir)
	t.Setenv("BRAID_GLOBAL_DATA", dir)

	require.NoError(t, os.WriteFile(configPath, []byte("{}"), 0o600))

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})
	require.NotEmpty(t, store.knownProviders, "embedded catalog should have loaded")

	token := &oauth.Token{
		AccessToken:  "copilot-at",
		RefreshToken: "copilot-rt",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}
	require.NoError(t, store.SetProviderAPIKey(ScopeGlobal, "copilot", token))

	raw, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, "copilot-at", gjson.GetBytes(raw, "providers.copilot.api_key").String())
	require.True(t, gjson.GetBytes(raw, "providers.copilot.oauth").Exists())
	require.False(t, gjson.GetBytes(raw, "providers.copilot.base_url").Exists(),
		"catalog provider must not get base_url pinned into the user config")
	require.False(t, gjson.GetBytes(raw, "providers.copilot.type").Exists(),
		"catalog provider must not get type pinned into the user config")
}

// TestSetProviderAPIKey_UnknownProviderLeavesNoDiskTrace covers fix 2.9:
// SetProviderAPIKey validates that the provider is known (or already
// configured) before writing anything to disk, so a failed call leaves the
// config file untouched.
func TestSetProviderAPIKey_UnknownProviderLeavesNoDiskTrace(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	t.Setenv("BRAID_GLOBAL_CONFIG", dir)
	t.Setenv("BRAID_GLOBAL_DATA", dir)

	const initialConfig = `{"options": {"debug": false}}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	before, err := os.ReadFile(configPath)
	require.NoError(t, err)

	err = store.SetProviderAPIKey(ScopeGlobal, "totally-unknown-provider", "some-api-key")
	require.Error(t, err)

	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "a failed SetProviderAPIKey must not modify the config file")

	err = store.SetProviderAPIKey(ScopeGlobal, "totally-unknown-provider", &oauth.Token{AccessToken: "at"})
	require.Error(t, err)

	after, err = os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "a failed OAuth SetProviderAPIKey must not modify the config file")
}

// TestReloadFromDiskLocked_DiscoveryDoesNotBlockWriteMu is a regression test
// for configureProviders' model-discovery step holding writeMu for the full
// duration of the HTTP round trip. reloadFromDisk (and Load) run
// configureProviders while holding writeMu, so a slow discovery endpoint
// currently blocks every other config mutator (SetConfigField,
// UpdatePreferredModel, ...) until discovery finishes or times out.
//
// EXPECTED TO FAIL against the current implementation: TryLock below returns
// false because writeMu is still held by the in-flight reload's discovery
// call. It is expected to pass once the writeMu/discovery refactor releases
// the lock before (or drops it during) the network round trip.
func TestReloadFromDiskLocked_DiscoveryDoesNotBlockWriteMu(t *testing.T) {
	const serverDelay = 200 * time.Millisecond

	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	// Isolate from the host's global config.
	t.Setenv("BRAID_GLOBAL_CONFIG", dir)
	t.Setenv("BRAID_GLOBAL_DATA", dir)

	// Start with no providers so the initial Load is fast and has nothing
	// to discover.
	require.NoError(t, os.WriteFile(configPath, []byte(`{}`), 0o600))

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// A slow discovery endpoint: sleeps well past our poll window before
	// responding with a minimal, valid models list.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(serverDelay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "slow-model", "object": "model"}]}`))
	}))
	defer server.Close()

	// Rewrite the config to add a custom provider with no models, which
	// auto-triggers discovery against the slow server on the next reload.
	slowConfig := fmt.Sprintf(`{
		"providers": {
			"custom": {
				"api_key": "test-key",
				"base_url": %q
			}
		}
	}`, server.URL+"/v1")
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(slowConfig), 0o600))

	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- store.ReloadFromDisk(context.Background())
	}()

	// Give the reload goroutine time to enter the HTTP call, but stay well
	// under serverDelay so we're observing mid-discovery state.
	time.Sleep(50 * time.Millisecond)

	tryStart := time.Now()
	acquired := store.writeMu.TryLock()
	tryElapsed := time.Since(tryStart)

	require.Less(t, tryElapsed, 50*time.Millisecond, "TryLock itself should return immediately, not block")
	if acquired {
		store.writeMu.Unlock()
	}
	require.True(t, acquired, "writeMu should not be held for the full discovery HTTP round trip; "+
		"other config mutators must remain usable while discovery is in flight")

	// Regardless of the assertion above, confirm the reload eventually
	// completes successfully so the test doesn't leak a goroutine. Receiving
	// from reloadDone is enough to synchronize: ReloadFromDisk releases
	// writeMu before it returns (see reloadFromDisk), so the send happens
	// after the unlock.
	require.NoError(t, <-reloadDone)
}

// TestLoad_AppleTerminalDefaultSurvivesReload pins down whether the
// isAppleTerminal-driven Options.TUI.Transparent default (set only in Load,
// via load.go's isAppleTerminal() block) survives a subsequent
// ReloadFromDisk. reloadFromDisk calls cfg.setDefaults but has no
// equivalent isAppleTerminal block, so the default may be lost on reload.
func TestLoad_AppleTerminalDefaultSurvivesReload(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")

	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	t.Setenv("BRAID_GLOBAL_CONFIG", dir)
	t.Setenv("BRAID_GLOBAL_DATA", dir)

	// Empty config: Load tolerates !cfg.IsConfigured() and returns early,
	// which keeps this test focused on the Apple Terminal default rather
	// than provider setup.
	require.NoError(t, os.WriteFile(configPath, []byte(`{}`), 0o600))

	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	require.NotNil(t, store.config.Options.TUI.Transparent)
	require.True(t, *store.config.Options.TUI.Transparent, "Load should enable transparent mode under Apple Terminal")

	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Force a reload without going through an unrelated mutator, so any
	// loss of the default is attributable to reloadFromDisk itself.
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(`{"options": {"debug": true}}`), 0o600))
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	cfg := store.Config()
	require.NotNil(t, cfg.Options.TUI.Transparent, "Apple Terminal transparent default should survive reload")
	require.True(t, cfg.Options.TUI.Transparent != nil && *cfg.Options.TUI.Transparent,
		"Apple Terminal transparent default should still be true after reload")
}

// TestReloadFromDisk_PublishedConfigNotMutated verifies the immutability
// invariant: reloadFromDisk must not mutate a previously published Config
// snapshot. It adds a markdown agent on disk between Load and Reload so
// the new Config differs from the old one; if SetupAgents runs after
// setConfig the old snapshot would gain the new agent and this test fails.
func TestReloadFromDisk_PublishedConfigNotMutated(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "braid.json")

	t.Setenv("BRAID_GLOBAL_CONFIG", dir)
	t.Setenv("BRAID_GLOBAL_DATA", dir)

	require.NoError(t, os.WriteFile(configPath, []byte(`{
		"model": {"provider": "openai", "model": "gpt-4"},
		"providers": {
			"openai": {
				"api_key": "test-key",
				"models": [{"id": "gpt-4", "name": "GPT-4"}]
			}
		}
	}`), 0o600))

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	oldCfg := store.Config()
	require.Contains(t, oldCfg.Agents, "coder")
	require.Contains(t, oldCfg.Agents, "task")

	// Drop a new markdown agent on disk so reload discovers it.
	agentID := "reviewer"
	agentPath := filepath.Join(dir, ".braid", "agents", agentID+".md")
	require.NoError(t, os.MkdirAll(filepath.Dir(agentPath), 0o755))
	require.NoError(t, os.WriteFile(agentPath, []byte(`---
name: reviewer
description: Reviews Go code
---
You review Go code.`), 0o644))

	// Reload directly — no staleness dance needed.
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	// OLD snapshot must NOT have the new agent or its tool in AllowedTools.
	require.NotContains(t, oldCfg.Agents, agentID,
		"pre-reload snapshot must not gain new agent")
	coderTools := oldCfg.Agents["coder"].AllowedTools
	require.NotContains(t, coderTools, agentID,
		"pre-reload snapshot coder AllowedTools must not gain new agent")

	// NEW config must have the agent and its tool.
	newCfg := store.Config()
	require.True(t, newCfg != oldCfg, "reload should publish a new Config pointer")
	require.Contains(t, newCfg.Agents, agentID, "reloaded config must have new agent")
	newCoderTools := newCfg.Agents["coder"].AllowedTools
	require.Contains(t, newCoderTools, agentID,
		"reloaded config coder AllowedTools must include new agent")
}

// TestOnboarding_FirstCredentialBuildsAgents reproduces the fresh-install
// flow: Load returns early (no provider configured, no SetupAgents), then
// onboarding writes the first credential and preferred model. The first
// config mutation that makes the store configured must build the agents
// map on the new snapshot — InitCoderAgent runs right after onboarding and
// needs Agents["coder"] — without mutating any previously published Config.
func TestOnboarding_FirstCredentialBuildsAgents(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRAID_GLOBAL_CONFIG", dir)
	t.Setenv("BRAID_GLOBAL_DATA", dir)

	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	preCfg := store.Config()
	require.False(t, preCfg.IsConfigured())
	require.Empty(t, preCfg.Agents, "unconfigured store must not have agents yet")

	require.NoError(t, store.SetProviderAPIKey(ScopeGlobal, "openai", "test-key"))

	cfg := store.Config()
	require.True(t, cfg.IsConfigured())
	require.Contains(t, cfg.Agents, "coder", "first credential write must build agents")
	require.Contains(t, cfg.Agents, "task")
	require.Empty(t, preCfg.Agents, "pre-onboarding snapshot must stay untouched")
}
