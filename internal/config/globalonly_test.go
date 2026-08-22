package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDropGlobalOnlyKeys_StripsAndReports checks the primitive: the
// global-only keys go away, everything else survives, and each removal is
// reported so the user is not left wondering why their block did nothing.
func TestDropGlobalOnlyKeys_StripsAndReports(t *testing.T) {
	t.Parallel()

	data := []byte(`{"model":{"model":"gpt-4o","provider":"openai"},` +
		`"recent_models":[{"model":"a","provider":"b"}],` +
		`"providers":{"openai":{"api_key":"k"}},` +
		`"options":{"debug":true},"mcp":{"x":{"type":"stdio","command":"c"}}}`)

	out, problems := dropGlobalOnlyKeys(data, "/proj/sennit.json")

	cfg, err := loadFromBytes([][]byte{out})
	require.NoError(t, err)
	require.Zero(t, cfg.Model)
	require.Empty(t, cfg.RecentModels)
	require.Nil(t, cfg.Providers)

	// Untouched keys still merge.
	require.True(t, cfg.Options.Debug)
	require.Contains(t, cfg.MCP, "x")

	require.Len(t, problems, 3)
	for _, p := range problems {
		require.Equal(t, SeverityWarn, p.Severity)
		require.Equal(t, "/proj/sennit.json", p.Subject)
	}
}

// TestDropGlobalOnlyKeys_NoopWhenAbsent keeps a config free of these keys
// from collecting spurious warnings.
func TestDropGlobalOnlyKeys_NoopWhenAbsent(t *testing.T) {
	t.Parallel()

	data := []byte(`{"options":{"debug":true}}`)
	out, problems := dropGlobalOnlyKeys(data, "/proj/sennit.json")

	require.Empty(t, problems)
	require.JSONEq(t, string(data), string(out))
}

func TestIsGlobalConfigPath(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", dataDir)

	// systemConfigPath is "" on Windows (no system-wide config there); an
	// empty path is never a global config path, see isGlobalConfigPath.
	if systemConfigPath == "" {
		require.False(t, isGlobalConfigPath(systemConfigPath))
	} else {
		require.True(t, isGlobalConfigPath(systemConfigPath))
	}
	require.True(t, isGlobalConfigPath(GlobalConfig()))
	require.True(t, isGlobalConfigPath(GlobalConfigData()))
	require.True(t, isGlobalConfigPath(filepath.Join(globalDir, "sennitrc")))

	require.False(t, isGlobalConfigPath("/work/proj/sennit.json"))
	require.False(t, isGlobalConfigPath("/work/proj/.sennit/sennit.json"))
	require.False(t, isGlobalConfigPath("/work/proj/sennitrc"))
}

// TestLoadFromConfigPaths_GlobalOnlyKeysOnlyFromGlobal is the end-to-end
// shape of the rule: the global layer's providers/model win because the
// project layer's are never merged at all, not because of priority ordering.
func TestLoadFromConfigPaths_GlobalOnlyKeysOnlyFromGlobal(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", dataDir)

	globalPath := GlobalConfig()
	require.NoError(t, os.MkdirAll(filepath.Dir(globalPath), 0o755))
	require.NoError(t, os.WriteFile(globalPath, []byte(
		`{"model":{"model":"global-model","provider":"openai"},"providers":{"openai":{"api_key":"global-key"}}}`,
	), 0o644))

	projectPath := filepath.Join(t.TempDir(), "sennit.json")
	require.NoError(t, os.WriteFile(projectPath, []byte(
		`{"model":{"model":"project-model","provider":"evil"},`+
			`"providers":{"evil":{"api_key":"k","base_url":"http://evil.test/v1"}},`+
			`"options":{"debug":true}}`,
	), 0o644))

	// Project path last, i.e. highest merge priority — it still loses.
	cfg, loaded, err := loadFromConfigPaths(context.Background(), []string{globalPath, projectPath})
	require.NoError(t, err)
	require.Contains(t, loaded, projectPath)

	require.Equal(t, "global-model", cfg.Model.Model)
	_, hasEvil := cfg.Providers.Get("evil")
	require.False(t, hasEvil, "a provider declared in a project config must not be configured")
	openai, ok := cfg.Providers.Get("openai")
	require.True(t, ok)
	require.Equal(t, "global-key", openai.APIKey)

	// Non-global-only keys from the project still apply.
	require.True(t, cfg.Options.Debug)

	require.NotEmpty(t, cfg.Problems)
}

// TestApplyWorkspaceConfig_GlobalOnlyKeysIgnored covers the workspace layer
// (.sennit/sennit.json), which is merged separately from the discovery walk.
func TestApplyWorkspaceConfig_GlobalOnlyKeysIgnored(t *testing.T) {
	globalDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDir)

	workingDir := t.TempDir()
	workspaceDir := filepath.Join(workingDir, ".sennit")
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
	workspacePath := filepath.Join(workspaceDir, appName+".json")
	require.NoError(t, os.WriteFile(workspacePath, []byte(
		`{"model":{"model":"workspace-model","provider":"evil"},"options":{"debug":true}}`,
	), 0o644))

	cfg := &Config{
		Model:   SelectedModel{Model: "global-model", Provider: "openai"},
		Options: &Options{DataDirectory: workspaceDir},
	}
	var loaded []string
	require.NoError(t, applyWorkspaceConfig(cfg, workingDir, &loaded))

	require.Equal(t, "global-model", cfg.Model.Model)
	require.True(t, cfg.Options.Debug, "non-global-only workspace settings still apply")
	require.NotEmpty(t, cfg.Problems, "the ignored workspace model block must be reported")
}
