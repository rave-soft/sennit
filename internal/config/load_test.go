package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config/migrate"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/testenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Fail safe: a test in this package that forgets to point the global
	// profile somewhere disposable would otherwise write into the
	// developer's real one.
	cleanup := testenv.IsolateGlobalProfile()
	exitVal := m.Run()
	cleanup()
	os.Exit(exitVal)
}

func TestConfig_LoadFromBytes(t *testing.T) {
	data1 := []byte(`{"providers": {"openai": {"api_key": "key1", "base_url": "https://api.openai.com/v1"}}}`)
	data2 := []byte(`{"providers": {"openai": {"api_key": "key2", "base_url": "https://api.openai.com/v2"}}}`)
	data3 := []byte(`{"providers": {"openai": {}}}`)

	loadedConfig, err := loadFromBytes([][]byte{data1, data2, data3})

	require.NoError(t, err)
	require.NotNil(t, loadedConfig)
	require.Equal(t, 1, loadedConfig.Providers.Len())
	pc, _ := loadedConfig.Providers.Get("openai")
	require.Equal(t, "key2", pc.APIKey)
	require.Equal(t, "https://api.openai.com/v2", pc.BaseURL)
}

func TestConfig_LoadFromBytes_ThreadsWorktreeDir(t *testing.T) {
	data := []byte(`{"options":{"threads":{"worktree_dir":"../thread-worktrees"}},"providers": {}}`)

	loadedConfig, err := loadFromBytes([][]byte{data})

	require.NoError(t, err)
	require.NotNil(t, loadedConfig.Options.Threads)
	require.Equal(t, "../thread-worktrees", loadedConfig.Options.Threads.WorktreeDir)
}

// TestConfig_LoadFromBytes_SingleModel verifies that a top-level "model" key
// (the replacement for the old "models": {"large": ...} shape) unmarshals
// into cfg.Model.
func TestConfig_LoadFromBytes_SingleModel(t *testing.T) {
	data := []byte(`{"model":{"provider":"openai","model":"gpt-4o"},"providers": {}}`)

	loadedConfig, err := loadFromBytes([][]byte{data})

	require.NoError(t, err)
	require.Equal(t, "openai", loadedConfig.Model.Provider)
	require.Equal(t, "gpt-4o", loadedConfig.Model.Model)
}

// TestConfig_LoadFromBytes_DropsIncompatibleRecentModels verifies that a
// pre-refactor "recent_models" value (an object keyed by "large"/"small")
// is dropped instead of failing json.Unmarshal, so old data-dir configs
// don't stop sennit from starting.
func TestConfig_LoadFromBytes_DropsIncompatibleRecentModels(t *testing.T) {
	data := []byte(`{"recent_models":{"large":[{"provider":"openai","model":"gpt-4o"}]},"providers": {}}`)
	data = migrate.DropIncompatibleRecentModels(data, "test.json")

	loadedConfig, err := loadFromBytes([][]byte{data})

	require.NoError(t, err)
	require.Empty(t, loadedConfig.RecentModels)
}

func TestApplyWorkspaceConfig(t *testing.T) {
	workingDir := t.TempDir()
	workspaceDir := filepath.Join(t.TempDir(), "workspace-data")
	workspacePath := filepath.Join(workspaceDir, appName+".json")
	newConfig := func() *Config {
		cfg := &Config{}
		cfg.setDefaults(workingDir, workspaceDir)
		return cfg
	}

	t.Run("missing and empty are no-ops", func(t *testing.T) {
		cfg := newConfig()
		var loaded []string
		require.NoError(t, applyWorkspaceConfig(cfg, workingDir, &loaded))
		require.Empty(t, loaded)
		require.False(t, cfg.Options.Debug)

		require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
		require.NoError(t, os.WriteFile(workspacePath, nil, 0o644))
		require.NoError(t, applyWorkspaceConfig(cfg, workingDir, &loaded))
		require.Empty(t, loaded)
		require.False(t, cfg.Options.Debug)
	})

	t.Run("read error includes path and cause", func(t *testing.T) {
		cfg := newConfig()
		var loaded []string
		require.NoError(t, os.RemoveAll(workspaceDir))
		require.NoError(t, os.WriteFile(workspaceDir, nil, 0o644))

		err := applyWorkspaceConfig(cfg, workingDir, &loaded)
		require.Error(t, err)
		require.Contains(t, err.Error(), workspacePath)
		var pathErr *os.PathError
		require.True(t, errors.As(err, &pathErr))
		require.Equal(t, "open", pathErr.Op)
		require.Equal(t, workspacePath, pathErr.Path)
		require.Error(t, pathErr.Err)
		require.Empty(t, loaded)
		require.NoError(t, os.Remove(workspaceDir))
	})

	t.Run("valid config merges and retains data directory and agents marker", func(t *testing.T) {
		cfg := newConfig()
		cfg.jsonAgentsBlockDetected = true
		var loaded []string
		require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
		require.NoError(t, os.WriteFile(workspacePath, []byte(`{"options":{"debug":true}}`), 0o644))

		require.NoError(t, applyWorkspaceConfig(cfg, workingDir, &loaded))
		require.True(t, cfg.Options.Debug)
		require.Equal(t, workspaceDir, cfg.Options.DataDirectory)
		require.True(t, cfg.jsonAgentsBlockDetected)
		require.Equal(t, []string{workspacePath}, loaded)
	})

	t.Run("invalid JSON returns the established error", func(t *testing.T) {
		cfg := newConfig()
		var loaded []string
		require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
		require.NoError(t, os.WriteFile(workspacePath, []byte(`{invalid`), 0o644))

		err := applyWorkspaceConfig(cfg, workingDir, &loaded)
		require.EqualError(t, err, "invalid JSON in config file "+workspacePath)
		require.Empty(t, loaded)
		require.False(t, cfg.Options.Debug)
	})

	t.Run("merge error warns and leaves the base unchanged", func(t *testing.T) {
		cfg := newConfig()
		var loaded []string
		require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
		require.NoError(t, os.WriteFile(workspacePath, []byte(`{"options":"invalid"}`), 0o644))

		require.NoError(t, applyWorkspaceConfig(cfg, workingDir, &loaded))
		require.False(t, cfg.Options.Debug)
		require.Equal(t, workspaceDir, cfg.Options.DataDirectory)
		require.Empty(t, loaded)
	})
}

func TestLoad_WorkspaceMergePreservesAgentsMarker(t *testing.T) {
	workingDir := t.TempDir()
	globalDir := t.TempDir()
	workspaceDir := filepath.Join(t.TempDir(), "custom-workspace-data")
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDir)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, appName+".json"), []byte(`{"agents":{},"options":{"data_directory":"`+workspaceDir+`"}}`), 0o644))

	workspacePath := filepath.Join(workspaceDir, appName+".json")
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
	require.NoError(t, os.WriteFile(workspacePath, []byte(`{"options":{"debug":true}}`), 0o644))

	store, err := Load(workingDir, "", false)
	require.NoError(t, err)
	require.True(t, store.Config().jsonAgentsBlockDetected)
}

func TestLoad_WorkspaceLegacyRecentModelsPreservesSiblingFields(t *testing.T) {
	workingDir := t.TempDir()
	globalDir := t.TempDir()
	workspaceDir := filepath.Join(t.TempDir(), "custom-workspace-data")
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDir)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, appName+".json"), []byte(`{"options":{"data_directory":"`+workspaceDir+`"}}`), 0o644))

	workspacePath := filepath.Join(workspaceDir, appName+".json")
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
	require.NoError(t, os.WriteFile(workspacePath, []byte(`{"recent_models":{"large":[]},"options":{"debug":true}}`), 0o644))

	store, err := Load(workingDir, "", false)
	require.NoError(t, err)
	require.True(t, store.Config().Options.Debug)
	require.Empty(t, store.Config().RecentModels)
	require.Equal(t, workspaceDir, store.Config().Options.DataDirectory)
	require.Contains(t, store.LoadedPaths(), workspacePath)
}

// TestConfig_LoadFromBytes_DeprecatedStrandsAlias verifies that the old
// "options.strands" key (from before the strands→threads rename) still
// populates Options.Threads, so existing sennit.json/sennitrc files keep
// working without edits.
func TestConfig_LoadFromBytes_DeprecatedStrandsAlias(t *testing.T) {
	data := []byte(`{"options":{"strands":{"worktree_dir":"../thread-worktrees"}},"providers": {}}`)
	data = migrate.MigrateDeprecatedKey(data, "options.strands", "options.threads", "test.json")

	loadedConfig, err := loadFromBytes([][]byte{data})

	require.NoError(t, err)
	require.NotNil(t, loadedConfig.Options.Threads)
	require.Equal(t, "../thread-worktrees", loadedConfig.Options.Threads.WorktreeDir)
}

// TestConfig_LoadFromConfigPaths_DeprecatedStrandsAlias exercises the
// alias through the full file-loading path.
func TestConfig_LoadFromConfigPaths_DeprecatedStrandsAlias(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sennit.json")
	require.NoError(t, os.WriteFile(
		path,
		[]byte(`{"options":{"strands":{"worktree_dir":"../thread-worktrees"}},"providers": {}}`),
		0o644,
	))

	cfg, loaded, err := loadFromConfigPaths(context.Background(), []string{path})

	require.NoError(t, err)
	require.Contains(t, loaded, path)
	require.NotNil(t, cfg.Options.Threads)
	require.Equal(t, "../thread-worktrees", cfg.Options.Threads.WorktreeDir)
}

func TestLookupConfigs_BoundedByProject(t *testing.T) {
	// Force GlobalConfig and GlobalConfigData to point at locations we
	// control so they can be present in the result without polluting
	// the developer's real config.
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", dataDir)

	t.Run("does not pick up sennit.json above non-git project", func(t *testing.T) {
		parent := t.TempDir()

		// sennit.json above the project must not be adopted.
		require.NoError(t, os.WriteFile(
			filepath.Join(parent, "sennit.json"),
			[]byte(`{}`),
			0o644,
		))

		project := filepath.Join(parent, "project")
		require.NoError(t, os.Mkdir(project, 0o755))

		got := lookupConfigs(project)
		for _, p := range got {
			require.NotEqual(t, filepath.Join(parent, "sennit.json"), p)
		}
	})

	t.Run("does not climb out of git worktree to find sennit.json", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not available")
		}

		parent := t.TempDir()

		require.NoError(t, os.WriteFile(
			filepath.Join(parent, "sennit.json"),
			[]byte(`{}`),
			0o644,
		))

		worktree := filepath.Join(parent, "worktree")
		require.NoError(t, os.Mkdir(worktree, 0o755))
		gitInit := exec.CommandContext(t.Context(), "git", "init", "-q")
		gitInit.Dir = worktree
		require.NoError(t, gitInit.Run())

		got := lookupConfigs(worktree)
		strayEval, err := filepath.EvalSymlinks(filepath.Join(parent, "sennit.json"))
		require.NoError(t, err)
		for _, p := range got {
			pEval, err := filepath.EvalSymlinks(p)
			if err != nil {
				continue
			}
			require.NotEqual(t, strayEval, pEval, "must not adopt parent sennit.json")
		}
	})

	t.Run("picks up sennit.json inside the project", func(t *testing.T) {
		project := t.TempDir()
		local := filepath.Join(project, "sennit.json")
		require.NoError(t, os.WriteFile(local, []byte(`{}`), 0o644))

		got := lookupConfigs(project)

		localEval, err := filepath.EvalSymlinks(local)
		require.NoError(t, err)
		var foundLocal bool
		for _, p := range got {
			pEval, err := filepath.EvalSymlinks(p)
			if err != nil {
				continue
			}
			if pEval == localEval {
				foundLocal = true
				break
			}
		}
		require.True(t, foundLocal, "expected project sennit.json to be in lookup result: %v", got)
	})

	t.Run("global config is always included regardless of boundary", func(t *testing.T) {
		project := t.TempDir()

		got := lookupConfigs(project)
		// Global config and global data path are always prepended,
		// even when no project file exists.
		require.Contains(t, got, GlobalConfig())
		require.Contains(t, got, GlobalConfigData())
	})

	t.Run("global shell config (sennitrc) is included", func(t *testing.T) {
		project := t.TempDir()

		got := lookupConfigs(project)
		// A global sennitrc is discovered only beside the user config. The data
		// directory is machine-owned state and must never execute a sennitrc.
		require.Contains(t, got, shellConfigSibling(GlobalConfig()))
		require.NotContains(t, got, shellConfigSibling(GlobalConfigData()))
	})

	t.Run("project sennitrc and .sennitrc are discovered", func(t *testing.T) {
		project := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(project, "sennitrc"), []byte(""), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(project, ".sennitrc"), []byte(""), 0o644))

		got := lookupConfigs(project)
		require.Contains(t, got, filepath.Join(project, "sennitrc"))
		require.Contains(t, got, filepath.Join(project, ".sennitrc"))
	})

	t.Run(".sennit/sennit.json and .sennit/sennitrc are discovered", func(t *testing.T) {
		project := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(project, ".sennit"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(project, ".sennit", "sennit.json"), []byte("{}"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(project, ".sennit", "sennitrc"), []byte(""), 0o644))

		got := lookupConfigs(project)
		require.Contains(t, got, filepath.Join(project, ".sennit", "sennit.json"))
		require.Contains(t, got, filepath.Join(project, ".sennit", "sennitrc"))
	})

	t.Run(".sennit/sennit.json outranks root sennit.json and .sennit.json", func(t *testing.T) {
		project := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(project, "sennit.json"),
			[]byte(`{"options":{"data_directory":"from-root-json"}}`), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(project, ".sennit.json"),
			[]byte(`{"options":{"data_directory":"from-dot-json"}}`), 0o644))
		require.NoError(t, os.MkdirAll(filepath.Join(project, ".sennit"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(project, ".sennit", "sennit.json"),
			[]byte(`{"options":{"data_directory":"from-sennit-subdir"}}`), 0o644))

		cfg, _, err := loadFromConfigPaths(context.Background(), lookupConfigs(project))
		require.NoError(t, err)
		require.Equal(t, "from-sennit-subdir", cfg.Options.DataDirectory)
	})

	t.Run(".sennit/sennitrc outranks root sennitrc, .sennitrc, and .sennit/sennit.json", func(t *testing.T) {
		project := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(project, "sennitrc"),
			[]byte("option data-directory from-root-sennitrc\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(project, ".sennitrc"),
			[]byte("option data-directory from-dot-sennitrc\n"), 0o644))
		require.NoError(t, os.MkdirAll(filepath.Join(project, ".sennit"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(project, ".sennit", "sennit.json"),
			[]byte(`{"options":{"data_directory":"from-sennit-subdir-json"}}`), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(project, ".sennit", "sennitrc"),
			[]byte("option data-directory from-sennit-subdir-rc\n"), 0o644))

		cfg, _, err := loadFromConfigPaths(context.Background(), lookupConfigs(project))
		require.NoError(t, err)
		require.Equal(t, "from-sennit-subdir-rc", cfg.Options.DataDirectory)
	})

	t.Run("system config is loaded first", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("system config not supported on Windows")
		}

		got := lookupConfigs(t.TempDir())
		require.NotEmpty(t, got)
		// The system-wide config must be first so it has the lowest
		// priority when configs are merged.
		require.Equal(t, "/etc/sennit/sennit.json", got[0])
	})
}

func TestLoadFromConfigPaths_InvalidJSON(t *testing.T) {
	t.Parallel()

	t.Run("identifies the offending file", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		good := filepath.Join(tmpDir, "good.json")
		bad := filepath.Join(tmpDir, "bad.json")
		require.NoError(t, os.WriteFile(good, []byte(`{"providers":{}}`), 0o644))
		require.NoError(t, os.WriteFile(bad, []byte(`{not valid json}`), 0o644))

		_, _, err := loadFromConfigPaths(context.Background(), []string{good, bad})
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid JSON in config file")
		require.Contains(t, err.Error(), "bad.json")
	})

	t.Run("skips missing and empty files", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		empty := filepath.Join(tmpDir, "empty.json")
		require.NoError(t, os.WriteFile(empty, []byte(""), 0o644))

		cfg, _, err := loadFromConfigPaths(context.Background(), []string{
			filepath.Join(tmpDir, "nonexistent.json"),
			empty,
		})
		require.NoError(t, err)
		require.NotNil(t, cfg)
	})
}

// TestLoadFromConfigPaths_ConflictWarningNamesKeys verifies that when a JSON
// config and a sennitrc coexist in the same directory, the merge warning names
// the overlapping top-level keys so incremental migrations can spot stale
// duplicates.
func TestLoadFromConfigPaths_ConflictWarningNamesKeys(t *testing.T) {
	capture := func(t *testing.T) *strings.Builder {
		t.Helper()
		var buf strings.Builder
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		t.Cleanup(func() { slog.SetDefault(prev) })
		return &buf
	}

	t.Run("names overlapping keys", func(t *testing.T) {
		buf := capture(t)
		tmpDir := t.TempDir()
		jsonPath := filepath.Join(tmpDir, "sennit.json")
		rcPath := filepath.Join(tmpDir, "sennitrc")
		require.NoError(t, os.WriteFile(jsonPath, []byte(`{"options":{"debug":true},"providers":{}}`), 0o644))
		require.NoError(t, os.WriteFile(rcPath, []byte("option debug true\n"), 0o644))

		_, _, err := loadFromConfigPaths(context.Background(), []string{jsonPath, rcPath})
		require.NoError(t, err)
		require.Contains(t, buf.String(), "sennitrc taking precedence")
		require.Contains(t, buf.String(), `"conflicting_keys":"options"`)
	})

	t.Run("no warning when nothing overlaps", func(t *testing.T) {
		buf := capture(t)
		tmpDir := t.TempDir()
		jsonPath := filepath.Join(tmpDir, "sennit.json")
		rcPath := filepath.Join(tmpDir, "sennitrc")
		require.NoError(t, os.WriteFile(jsonPath, []byte(`{"providers":{}}`), 0o644))
		require.NoError(t, os.WriteFile(rcPath, []byte("option debug true\n"), 0o644))

		_, _, err := loadFromConfigPaths(context.Background(), []string{jsonPath, rcPath})
		require.NoError(t, err)
		require.NotContains(t, buf.String(), "sennitrc taking precedence",
			"disjoint coexistence should not warn")
	})
}

// testStore wraps a Config in a minimal ConfigStore for testing.
func testStore(cfg *Config) *ConfigStore {
	return &ConfigStore{config: cfg}
}

func TestConfig_setDefaults(t *testing.T) {
	t.Run("sets default data directory", func(t *testing.T) {
		cfg := &Config{}
		workingDir := t.TempDir()

		cfg.setDefaults(workingDir, "")

		require.NotNil(t, cfg.Options)
		require.NotNil(t, cfg.Options.TUI)
		require.NotNil(t, cfg.Options.ContextPaths)
		require.NotNil(t, cfg.Providers)
		require.NotNil(t, cfg.LSP)
		require.NotNil(t, cfg.MCP)
		require.Equal(t, filepath.Join(workingDir, ".sennit"), cfg.Options.DataDirectory)
		require.Equal(t, "AGENTS.md", cfg.Options.InitializeAs)
		for _, path := range defaultContextPaths {
			require.Contains(t, cfg.Options.ContextPaths, path)
		}
	})

	t.Run("prunes orphaned OAuth token MCP entries but keeps real ones", func(t *testing.T) {
		cfg := &Config{
			MCP: map[string]MCPConfig{
				"orphan":     {OAuthToken: &oauth.Token{AccessToken: "stale"}},
				"real-http":  {Type: MCPHttp, URL: "https://example.com/mcp", OAuthToken: &oauth.Token{AccessToken: "live"}},
				"real-stdio": {Type: MCPStdio, Command: "npx"},
				"malformed":  {Command: "npx"}, // missing type but has a command: surface the error, don't prune
			},
		}

		cfg.setDefaults(t.TempDir(), "")

		require.NotContains(t, cfg.MCP, "orphan", "orphaned token entry should be pruned")
		require.Contains(t, cfg.MCP, "real-http")
		require.Contains(t, cfg.MCP, "real-stdio")
		require.Contains(t, cfg.MCP, "malformed", "malformed entry should survive so its error surfaces")
	})

	t.Run("resolves relative configured data directory from working directory", func(t *testing.T) {
		cfg := &Config{Options: &Options{DataDirectory: "."}}
		workingDir := filepath.Join(t.TempDir(), "worktree")

		cfg.setDefaults(workingDir, "")

		require.Equal(t, workingDir, cfg.Options.DataDirectory)
	})

	t.Run("resolves relative flag data directory from working directory", func(t *testing.T) {
		cfg := &Config{}
		workingDir := filepath.Join(t.TempDir(), "worktree")

		cfg.setDefaults(workingDir, "./state")

		require.Equal(t, filepath.Join(workingDir, "state"), cfg.Options.DataDirectory)
	})

	t.Run("preserves absolute configured data directory", func(t *testing.T) {
		// Use a platform-appropriate absolute path so the test runs
		// the same way on POSIX and Windows.
		absDir := filepath.Join(t.TempDir(), "data")
		cfg := &Config{Options: &Options{DataDirectory: absDir}}

		cfg.setDefaults(filepath.Join(t.TempDir(), "worktree"), "")

		require.Equal(t, absDir, cfg.Options.DataDirectory)
	})

	t.Run("workspace merge re-entry keeps an absolute data directory", func(t *testing.T) {
		// Simulate the load and reload paths: defaults are applied
		// twice with the data directory potentially carried through
		// from an earlier merge as a relative string.
		workingDir := filepath.Join(t.TempDir(), "worktree")
		cfg := &Config{}
		cfg.setDefaults(workingDir, "")

		// Workspace JSON sets data_directory to a relative value; the
		// merge replaces the struct, then setDefaults runs again.
		cfg.Options.DataDirectory = "./state"
		cfg.setDefaults(workingDir, "")

		require.True(t, filepath.IsAbs(cfg.Options.DataDirectory),
			"data directory must remain absolute after re-merge, got %q",
			cfg.Options.DataDirectory)
		require.Equal(t, filepath.Join(workingDir, "state"), cfg.Options.DataDirectory)
	})

	t.Run("does not adopt .sennit from a parent project", func(t *testing.T) {
		parent := t.TempDir()

		// .sennit in the parent: it should not be reused by the child
		// because there is no git context joining them.
		require.NoError(t, os.Mkdir(filepath.Join(parent, defaultDataDirectory), 0o755))

		child := filepath.Join(parent, "child")
		require.NoError(t, os.Mkdir(child, 0o755))

		cfg := &Config{}
		cfg.setDefaults(child, "")

		require.Equal(
			t,
			filepath.Clean(filepath.Join(child, defaultDataDirectory)),
			filepath.Clean(cfg.Options.DataDirectory),
		)
	})

	t.Run("does not climb out of git worktree to find .sennit", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not available")
		}

		parent := t.TempDir()

		// Stray .sennit above the worktree root.
		require.NoError(t, os.Mkdir(filepath.Join(parent, defaultDataDirectory), 0o755))

		worktree := filepath.Join(parent, "worktree")
		require.NoError(t, os.Mkdir(worktree, 0o755))

		sub := filepath.Join(worktree, "pkg")
		require.NoError(t, os.Mkdir(sub, 0o755))

		// Make worktree a real git repo so the boundary detection
		// resolves to it, mirroring what happens with linked worktrees
		// in real usage.
		gitInit := exec.CommandContext(t.Context(), "git", "init", "-q")
		gitInit.Dir = worktree
		require.NoError(t, gitInit.Run())

		cfg := &Config{}
		cfg.setDefaults(sub, "")

		// Resolve symlinks because TempDir on macOS sits under /var
		// which is a symlink to /private/var. The data directory has
		// not been created yet, so resolve its parent and join.
		gotDir, gotName := filepath.Split(cfg.Options.DataDirectory)
		gotEvalDir, err := filepath.EvalSymlinks(filepath.Clean(gotDir))
		require.NoError(t, err)
		gotEval := filepath.Join(gotEvalDir, gotName)

		strayEval, err := filepath.EvalSymlinks(filepath.Join(parent, defaultDataDirectory))
		require.NoError(t, err)
		require.NotEqual(t, strayEval, gotEval, "must not adopt parent .sennit")

		subEval, err := filepath.EvalSymlinks(sub)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(subEval, defaultDataDirectory), gotEval)
	})
}

func TestConfig_configureProviders(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models: []catwalk.Model{{
				ID: "test-model",
			}},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")
	env := testenv.New(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	require.Equal(t, 1, cfg.Providers.Len())

	// We want to make sure that we keep the configured API key as a placeholder
	pc, _ := cfg.Providers.Get("openai")
	require.Equal(t, "$OPENAI_API_KEY", pc.APIKey)
}

func TestConfig_configureProvidersWithOverride(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models: []catwalk.Model{{
				ID: "test-model",
			}},
		},
	}

	cfg := &Config{
		Providers: csync.NewMap[string, ProviderConfig](),
	}
	cfg.Providers.Set("openai", ProviderConfig{
		APIKey:  "xyz",
		BaseURL: "https://api.openai.com/v2",
		Models: []catwalk.Model{
			{
				ID:   "test-model",
				Name: "Updated",
			},
			{
				ID: "another-model",
			},
		},
	})
	cfg.setDefaults("/tmp", "")

	env := testenv.New(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	require.Equal(t, 1, cfg.Providers.Len())

	// We want to make sure that we keep the configured API key as a placeholder
	pc, _ := cfg.Providers.Get("openai")
	require.Equal(t, "xyz", pc.APIKey)
	require.Equal(t, "https://api.openai.com/v2", pc.BaseURL)
	require.Len(t, pc.Models, 2)
	require.Equal(t, "Updated", pc.Models[0].Name)
}

func TestConfig_configureProvidersWithNewProvider(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models: []catwalk.Model{{
				ID: "test-model",
			}},
		},
	}

	cfg := &Config{
		Providers: csync.NewMap(map[string]ProviderConfig{
			"custom": {
				APIKey:  "xyz",
				BaseURL: "https://api.someendpoint.com/v2",
				Models: []catwalk.Model{
					{
						ID: "test-model",
					},
				},
			},
		}),
	}
	cfg.setDefaults("/tmp", "")
	env := testenv.New(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	// Should be to because of the env variable
	require.Equal(t, cfg.Providers.Len(), 2)

	// We want to make sure that we keep the configured API key as a placeholder
	pc, _ := cfg.Providers.Get("custom")
	require.Equal(t, "xyz", pc.APIKey)
	// Make sure we set the ID correctly
	require.Equal(t, "custom", pc.ID)
	require.Equal(t, "https://api.someendpoint.com/v2", pc.BaseURL)
	require.Len(t, pc.Models, 1)

	_, ok := cfg.Providers.Get("openai")
	require.True(t, ok, "OpenAI provider should still be present")
}

func TestConfig_configureProvidersBedrockWithCredentials(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          catwalk.InferenceProviderBedrock,
			APIKey:      "",
			APIEndpoint: "",
			Models: []catwalk.Model{{
				ID: "anthropic.claude-sonnet-4-20250514-v1:0",
			}},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")
	env := testenv.New(map[string]string{
		"AWS_ACCESS_KEY_ID":     "test-key-id",
		"AWS_SECRET_ACCESS_KEY": "test-secret-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	require.Equal(t, cfg.Providers.Len(), 1)

	bedrockProvider, ok := cfg.Providers.Get("bedrock")
	require.True(t, ok, "Bedrock provider should be present")
	require.Len(t, bedrockProvider.Models, 1)
	require.Equal(t, "anthropic.claude-sonnet-4-20250514-v1:0", bedrockProvider.Models[0].ID)
}

func TestConfig_configureProvidersBedrockWithoutCredentials(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          catwalk.InferenceProviderBedrock,
			APIKey:      "",
			APIEndpoint: "",
			Models: []catwalk.Model{{
				ID: "anthropic.claude-sonnet-4-20250514-v1:0",
			}},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")
	env := testenv.New(map[string]string{})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	// Provider should not be configured without credentials
	require.Equal(t, cfg.Providers.Len(), 0)
}

func TestConfig_configureProvidersVertexAIWithCredentials(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          catwalk.InferenceProviderVertexAI,
			APIKey:      "",
			APIEndpoint: "",
			Models: []catwalk.Model{{
				ID: "gemini-pro",
			}},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")
	env := testenv.New(map[string]string{
		"VERTEXAI_PROJECT":  "test-project",
		"VERTEXAI_LOCATION": "us-central1",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	require.Equal(t, cfg.Providers.Len(), 1)

	vertexProvider, ok := cfg.Providers.Get("vertexai")
	require.True(t, ok, "VertexAI provider should be present")
	require.Len(t, vertexProvider.Models, 1)
	require.Equal(t, "gemini-pro", vertexProvider.Models[0].ID)
	require.Equal(t, "test-project", vertexProvider.ExtraParams["project"])
	require.Equal(t, "us-central1", vertexProvider.ExtraParams["location"])
}

func TestConfig_configureProvidersVertexAIWithoutCredentials(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          catwalk.InferenceProviderVertexAI,
			APIKey:      "",
			APIEndpoint: "",
			Models: []catwalk.Model{{
				ID: "gemini-pro",
			}},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")
	env := testenv.New(map[string]string{
		"GOOGLE_GENAI_USE_VERTEXAI": "false",
		"GOOGLE_CLOUD_PROJECT":      "test-project",
		"GOOGLE_CLOUD_LOCATION":     "us-central1",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	// Provider should not be configured without proper credentials
	require.Equal(t, cfg.Providers.Len(), 0)
}

func TestConfig_configureProvidersVertexAIMissingProject(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          catwalk.InferenceProviderVertexAI,
			APIKey:      "",
			APIEndpoint: "",
			Models: []catwalk.Model{{
				ID: "gemini-pro",
			}},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")
	env := testenv.New(map[string]string{
		"GOOGLE_GENAI_USE_VERTEXAI": "true",
		"GOOGLE_CLOUD_LOCATION":     "us-central1",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	// Provider should not be configured without project
	require.Equal(t, cfg.Providers.Len(), 0)
}

func TestConfig_configureProvidersSetProviderID(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models: []catwalk.Model{{
				ID: "test-model",
			}},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")
	env := testenv.New(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	require.Equal(t, cfg.Providers.Len(), 1)

	// Provider ID should be set
	pc, _ := cfg.Providers.Get("openai")
	require.Equal(t, "openai", pc.ID)
}

func TestConfig_EnabledProviders(t *testing.T) {
	t.Run("all providers enabled", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"openai": {
					ID:      "openai",
					APIKey:  "key1",
					Disable: false,
				},
				"anthropic": {
					ID:      "anthropic",
					APIKey:  "key2",
					Disable: false,
				},
			}),
		}

		enabled := cfg.EnabledProviders()
		require.Len(t, enabled, 2)
	})

	t.Run("some providers disabled", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"openai": {
					ID:      "openai",
					APIKey:  "key1",
					Disable: false,
				},
				"anthropic": {
					ID:      "anthropic",
					APIKey:  "key2",
					Disable: true,
				},
			}),
		}

		enabled := cfg.EnabledProviders()
		require.Len(t, enabled, 1)
		require.Equal(t, "openai", enabled[0].ID)
	})

	t.Run("empty providers map", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap[string, ProviderConfig](),
		}

		enabled := cfg.EnabledProviders()
		require.Len(t, enabled, 0)
	})
}

func TestConfig_IsConfigured(t *testing.T) {
	t.Run("returns true when at least one provider is enabled", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"openai": {
					ID:      "openai",
					APIKey:  "key1",
					Disable: false,
				},
			}),
		}

		require.True(t, cfg.IsConfigured())
	})

	t.Run("returns false when no providers are configured", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap[string, ProviderConfig](),
		}

		require.False(t, cfg.IsConfigured())
	})

	t.Run("returns false when all providers are disabled", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"openai": {
					ID:      "openai",
					APIKey:  "key1",
					Disable: true,
				},
				"anthropic": {
					ID:      "anthropic",
					APIKey:  "key2",
					Disable: true,
				},
			}),
		}

		require.False(t, cfg.IsConfigured())
	})
}

func TestConfig_setupAgentsWithNoDisabledTools(t *testing.T) {
	cfg := &Config{
		Options: &Options{
			DisabledTools: []string{},
		},
	}

	cfg.SetupAgents()
	coderAgent, ok := cfg.Agents[AgentCoder]
	require.True(t, ok)
	assert.Equal(t, allToolNames(), coderAgent.AllowedTools)

	taskAgent, ok := cfg.Agents[AgentTask]
	require.True(t, ok)
	assert.Equal(t, []string{"lsp_symbols", "lsp_definition", "lsp_call_hierarchy", "fetch", "web_fetch", "web_search", "glob", "grep", "ripgrep", "ls", "read"}, taskAgent.AllowedTools)
}

func TestConfig_setupAgentsWithDisabledTools(t *testing.T) {
	cfg := &Config{
		Options: &Options{
			DisabledTools: []string{
				"edit",
				"download",
				"grep",
				"ripgrep",
			},
		},
	}

	cfg.SetupAgents()
	coderAgent, ok := cfg.Agents[AgentCoder]
	require.True(t, ok)

	assert.Equal(t, []string{"agent", "bash", "sennit_info", "sennit_logs", "job_output", "job_kill", "multiedit", "lsp_diagnostics", "lsp_references", "lsp_restart", "lsp_symbols", "lsp_definition", "lsp_call_hierarchy", "lsp_rename", "lsp_replace_symbol", "fetch", "agentic_fetch", "web_fetch", "web_search", "glob", "ls", "question", "todos", "read", "write", "list_mcp_resources", "read_mcp_resource", "thread_create", "thread_list", "thread_status", "thread_send", "thread_merge", "thread_remove", "task_list", "task_result", "task_cancel", "task_send", "task_output", "ask_parent"}, coderAgent.AllowedTools)

	taskAgent, ok := cfg.Agents[AgentTask]
	require.True(t, ok)
	assert.Equal(t, []string{"lsp_symbols", "lsp_definition", "lsp_call_hierarchy", "fetch", "web_fetch", "web_search", "glob", "ls", "read"}, taskAgent.AllowedTools)
}

func TestConfig_setupAgentsWithWebSearchDisabled(t *testing.T) {
	cfg := &Config{
		Options: &Options{
			DisabledTools: []string{
				"web_search",
			},
		},
	}

	cfg.SetupAgents()
	coderAgent, ok := cfg.Agents[AgentCoder]
	require.True(t, ok)

	assert.Contains(t, coderAgent.AllowedTools, "web_fetch")
	assert.NotContains(t, coderAgent.AllowedTools, "web_search")
}

func TestConfig_setupAgentsWithEveryReadOnlyToolDisabled(t *testing.T) {
	cfg := &Config{
		Options: &Options{
			DisabledTools: []string{
				"glob",
				"grep",
				"ripgrep",
				"ls",
				"lsp_call_hierarchy",
				"lsp_definition",
				"lsp_symbols",
				"read",
				"fetch",
				"web_fetch",
				"web_search",
			},
		},
	}

	cfg.SetupAgents()
	coderAgent, ok := cfg.Agents[AgentCoder]
	require.True(t, ok)
	assert.Equal(t, []string{"agent", "bash", "sennit_info", "sennit_logs", "job_output", "job_kill", "download", "edit", "multiedit", "lsp_diagnostics", "lsp_references", "lsp_restart", "lsp_rename", "lsp_replace_symbol", "agentic_fetch", "question", "todos", "write", "list_mcp_resources", "read_mcp_resource", "thread_create", "thread_list", "thread_status", "thread_send", "thread_merge", "thread_remove", "task_list", "task_result", "task_cancel", "task_send", "task_output", "ask_parent"}, coderAgent.AllowedTools)

	taskAgent, ok := cfg.Agents[AgentTask]
	require.True(t, ok)
	assert.Len(t, taskAgent.AllowedTools, 0)
}

func TestConfig_configureProvidersWithDisabledProvider(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models: []catwalk.Model{{
				ID: "test-model",
			}},
		},
	}

	cfg := &Config{
		Providers: csync.NewMap(map[string]ProviderConfig{
			"openai": {
				Disable: true,
			},
		}),
	}
	cfg.setDefaults("/tmp", "")

	env := testenv.New(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)

	require.Equal(t, cfg.Providers.Len(), 1)
	prov, exists := cfg.Providers.Get("openai")
	require.True(t, exists)
	require.True(t, prov.Disable)
}

func TestConfig_configureProvidersCustomProviderValidation(t *testing.T) {
	t.Run("custom provider with missing API key is allowed, but not known providers", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"custom": {
					BaseURL: "https://api.custom.com/v1",
					Models: []catwalk.Model{{
						ID: "test-model",
					}},
				},
				"openai": {
					APIKey: "$MISSING",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 1)
		_, exists := cfg.Providers.Get("custom")
		require.True(t, exists)
	})

	t.Run("custom provider with missing BaseURL is removed", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"custom": {
					APIKey: "test-key",
					Models: []catwalk.Model{{
						ID: "test-model",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 0)
		_, exists := cfg.Providers.Get("custom")
		require.False(t, exists)
	})

	t.Run("custom provider with no models attempts discovery and is removed on failure", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Models:  []catwalk.Model{},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		// Discovery fails (unreachable URL) so provider is removed.
		require.Equal(t, 0, cfg.Providers.Len())
		_, exists := cfg.Providers.Get("custom")
		require.False(t, exists)
	})

	t.Run("custom provider with no models and discover_models:false is removed", func(t *testing.T) {
		discoverFalse := false
		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"custom": {
					APIKey:             "test-key",
					BaseURL:            "https://api.custom.com/v1",
					Models:             []catwalk.Model{},
					AutoDiscoverModels: &discoverFalse,
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, 0, cfg.Providers.Len())
		_, exists := cfg.Providers.Get("custom")
		require.False(t, exists)
	})

	t.Run("custom provider with models and discover_models:true merges discovered models", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data": [
				{"id": "existing-model", "object": "model"},
				{"id": "discovered-model", "object": "model"}
			]}`))
		}))
		defer server.Close()

		discoverTrue := true
		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: server.URL + "/v1",
					Models: []catwalk.Model{
						{ID: "existing-model", Name: "My Custom Name", ContextWindow: 200000},
					},
					AutoDiscoverModels: &discoverTrue,
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, 1, cfg.Providers.Len())
		p, exists := cfg.Providers.Get("custom")
		require.True(t, exists)
		require.Len(t, p.Models, 2)

		// User-specified model keeps its custom fields.
		require.Equal(t, "existing-model", p.Models[0].ID)
		require.Equal(t, "My Custom Name", p.Models[0].Name)
		require.Equal(t, int64(200000), p.Models[0].ContextWindow)

		// Discovered model is appended.
		require.Equal(t, "discovered-model", p.Models[1].ID)
	})

	t.Run("custom provider with models and no discover_models uses only listed models", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Models: []catwalk.Model{
						{ID: "my-model", Name: "My Model"},
					},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, 1, cfg.Providers.Len())
		p, exists := cfg.Providers.Get("custom")
		require.True(t, exists)
		require.Len(t, p.Models, 1)
		require.Equal(t, "my-model", p.Models[0].ID)
	})

	t.Run("custom provider with no models auto-discovers successfully", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data": [
				{"id": "auto-model-a", "object": "model"},
				{"id": "auto-model-b", "object": "model"}
			]}`))
		}))
		defer server.Close()

		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: server.URL + "/v1",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, 1, cfg.Providers.Len())
		p, exists := cfg.Providers.Get("custom")
		require.True(t, exists)
		require.Len(t, p.Models, 2)
		require.Equal(t, "auto-model-a", p.Models[0].ID)
		require.Equal(t, "auto-model-b", p.Models[1].ID)
	})

	t.Run("custom provider with unsupported type is removed", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Type:    "unsupported",
					Models: []catwalk.Model{{
						ID: "test-model",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 0)
		_, exists := cfg.Providers.Get("custom")
		require.False(t, exists)
	})

	t.Run("valid custom provider is kept and ID is set", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Type:    catwalk.TypeOpenAI,
					Models: []catwalk.Model{{
						ID: "test-model",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 1)
		customProvider, exists := cfg.Providers.Get("custom")
		require.True(t, exists)
		require.Equal(t, "custom", customProvider.ID)
		require.Equal(t, "test-key", customProvider.APIKey)
		require.Equal(t, "https://api.custom.com/v1", customProvider.BaseURL)
	})

	t.Run("custom anthropic provider is supported", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"custom-anthropic": {
					APIKey:  "test-key",
					BaseURL: "https://api.anthropic.com/v1",
					Type:    catwalk.TypeAnthropic,
					Models: []catwalk.Model{{
						ID: "claude-3-sonnet",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 1)
		customProvider, exists := cfg.Providers.Get("custom-anthropic")
		require.True(t, exists)
		require.Equal(t, "custom-anthropic", customProvider.ID)
		require.Equal(t, "test-key", customProvider.APIKey)
		require.Equal(t, "https://api.anthropic.com/v1", customProvider.BaseURL)
		require.Equal(t, catwalk.TypeAnthropic, customProvider.Type)
	})

	t.Run("disabled custom provider is removed", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Type:    catwalk.TypeOpenAI,
					Disable: true,
					Models: []catwalk.Model{{
						ID: "test-model",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 0)
		_, exists := cfg.Providers.Get("custom")
		require.False(t, exists)
	})
}

// TestConfig_Load_DiscoveredModelsPersistAcrossReload verifies that a
// successful custom-provider model discovery is written to the data-dir
// config file, so a second Load of the same data dir finds a non-empty
// models list and skips the HTTP round trip entirely.
func TestConfig_Load_DiscoveredModelsPersistAcrossReload(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "auto-model", "object": "model"}]}`))
	}))
	defer server.Close()

	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", dataDir)

	// Seed the data-dir config (what GlobalConfigData() resolves to) with
	// a custom provider that has no models, so discovery auto-triggers.
	dataConfigPath := GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(dataConfigPath), 0o755))
	seed := fmt.Sprintf(`{"providers": {"custom": {"api_key": "test-key", "base_url": %q}}}`, server.URL+"/v1")
	require.NoError(t, os.WriteFile(dataConfigPath, []byte(seed), 0o644))

	workingDir1 := t.TempDir()
	store1, err := Load(workingDir1, "", false)
	require.NoError(t, err)
	pc, ok := store1.config.Providers.Get("custom")
	require.True(t, ok)
	require.Len(t, pc.Models, 1)
	require.Equal(t, "auto-model", pc.Models[0].ID)
	require.Equal(t, int64(1), requests.Load(), "first load should discover over HTTP")

	// The discovered models must now live in the model cache, not on disk
	// in the data-dir config.
	persisted, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(persisted, "providers.custom.models").Exists())
	cached, ok := loadCachedModels(dataConfigPath, "custom")
	require.True(t, ok)
	require.Len(t, cached, 1)
	require.Equal(t, "auto-model", cached[0].ID)

	// A second, independent Load of the same data dir must find the
	// models already populated (from the cache) and skip discovery
	// entirely.
	workingDir2 := t.TempDir()
	store2, err := Load(workingDir2, "", false)
	require.NoError(t, err)
	pc2, ok := store2.config.Providers.Get("custom")
	require.True(t, ok)
	require.Len(t, pc2.Models, 1)
	require.Equal(t, int64(1), requests.Load(), "second load must not re-discover over HTTP")
}

// TestConfig_Load_FailedDiscoveryLeavesDiskUntouched verifies that a custom
// provider whose discovery fails (unreachable endpoint) is dropped from the
// in-memory config as before, and that the failure never touches the
// data-dir config file: no bogus/empty models entry appears there.
func TestConfig_Load_FailedDiscoveryLeavesDiskUntouched(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", dataDir)

	dataConfigPath := GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(dataConfigPath), 0o755))
	seed := `{"providers": {"custom": {"api_key": "test-key", "base_url": "http://127.0.0.1:1/v1"}}}`
	require.NoError(t, os.WriteFile(dataConfigPath, []byte(seed), 0o644))
	before, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)

	workingDir := t.TempDir()
	store, err := Load(workingDir, "", false)
	require.NoError(t, err)
	_, exists := store.config.Providers.Get("custom")
	require.False(t, exists, "provider with failed discovery and no models must be dropped")

	after, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "failed discovery must not write to disk")
	require.False(t, gjson.GetBytes(after, "providers.custom.models").Exists())
}

// TestConfig_Load_ProjectModelsWinOverPersistedDataDirModels verifies that a
// project-level sennit.json's explicit, non-empty models list merges with a
// stale models list already persisted in the data-dir config, and that a
// non-empty merged list is enough to skip discovery entirely (no HTTP
// request is made). The stale data-dir models list is migrated out of the
// JSON file and into the model cache as a side effect of Load (see
// migrateBloatedModelCache); this test only cares that no HTTP discovery
// leaks in, not about that migration's own bookkeeping (covered by
// TestConfig_Load_MigratesBloatedModelCache).
func TestConfig_Load_ProjectModelsWinOverPersistedDataDirModels(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "discovered-model", "object": "model"}]}`))
	}))
	defer server.Close()

	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", dataDir)

	// Seed the data-dir config with a custom provider that already has a
	// persisted (stale) models list and a base_url.
	dataConfigPath := GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(dataConfigPath), 0o755))
	dataSeed := fmt.Sprintf(`{"providers": {"custom": {"api_key": "test-key", "base_url": %q, "models": [{"id": "stale-model", "name": "stale-model"}]}}}`, server.URL+"/v1")
	require.NoError(t, os.WriteFile(dataConfigPath, []byte(dataSeed), 0o644))

	// Seed a project-level sennit.json with a different, explicit models
	// list for the same provider ID. It intentionally omits base_url so we
	// can confirm the merge still inherits it from the data-dir file.
	workingDir := t.TempDir()
	projectSeed := `{"providers": {"custom": {"models": [{"id": "project-model", "name": "project-model"}]}}}`
	require.NoError(t, os.WriteFile(filepath.Join(workingDir, "sennit.json"), []byte(projectSeed), 0o644))

	store, err := Load(workingDir, "", false)
	require.NoError(t, err)
	pc, ok := store.config.Providers.Get("custom")
	require.True(t, ok)

	// jsons.Merge concatenates the array fields rather than letting the
	// higher-priority project file fully replace it, so the merged models
	// list carries both entries (data-dir's first, then the project's).
	// What matters for this test is that the project's model is present
	// and nothing from discovery leaked in.
	require.Len(t, pc.Models, 2)
	require.Equal(t, "stale-model", pc.Models[0].ID)
	require.Equal(t, "project-model", pc.Models[1].ID)

	// base_url is not set in the project file, so it must be inherited
	// from the data-dir config via field-by-field merge.
	require.Equal(t, server.URL+"/v1", pc.BaseURL)

	// A non-empty merged models list must short-circuit discovery.
	require.Equal(t, int64(0), requests.Load(), "discovery must not run when merged models is already non-empty")
}

func TestConfig_configureProvidersEnhancedCredentialValidation(t *testing.T) {
	t.Run("VertexAI provider removed when credentials missing with existing config", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:          catwalk.InferenceProviderVertexAI,
				APIKey:      "",
				APIEndpoint: "",
				Models: []catwalk.Model{{
					ID: "gemini-pro",
				}},
			},
		}

		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"vertexai": {
					BaseURL: "custom-url",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{
			"GOOGLE_GENAI_USE_VERTEXAI": "false",
		})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 0)
		_, exists := cfg.Providers.Get("vertexai")
		require.False(t, exists)
	})

	t.Run("Bedrock provider removed when AWS credentials missing with existing config", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:          catwalk.InferenceProviderBedrock,
				APIKey:      "",
				APIEndpoint: "",
				Models: []catwalk.Model{{
					ID: "anthropic.claude-sonnet-4-20250514-v1:0",
				}},
			},
		}

		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"bedrock": {
					BaseURL: "custom-url",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 0)
		_, exists := cfg.Providers.Get("bedrock")
		require.False(t, exists)
	})

	t.Run("provider removed when API key missing with existing config", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:          "openai",
				APIKey:      "$MISSING_API_KEY",
				APIEndpoint: "https://api.openai.com/v1",
				Models: []catwalk.Model{{
					ID: "test-model",
				}},
			},
		}

		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"openai": {
					BaseURL: "custom-url",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 0)
		_, exists := cfg.Providers.Get("openai")
		require.False(t, exists)
	})

	t.Run("known provider should still be added if the endpoint is missing the client will use default endpoints", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:          "openai",
				APIKey:      "$OPENAI_API_KEY",
				APIEndpoint: "$MISSING_ENDPOINT",
				Models: []catwalk.Model{{
					ID: "test-model",
				}},
			},
		}

		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"openai": {
					APIKey: "test-key",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{
			"OPENAI_API_KEY": "test-key",
		})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 1)
		_, exists := cfg.Providers.Get("openai")
		require.True(t, exists)
	})
}

func TestConfig_defaultModelSelection(t *testing.T) {
	t.Run("default behavior uses the default models for given provider", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{}
		cfg.setDefaults("/tmp", "")
		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		model, err := cfg.defaultModelSelection(knownProviders)
		require.NoError(t, err)
		require.Equal(t, "large-model", model.Model)
		require.Equal(t, "openai", model.Provider)
		require.Equal(t, int64(1000), model.MaxTokens)
	})
	t.Run("should error if no providers configured", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "$MISSING_KEY",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{}
		cfg.setDefaults("/tmp", "")
		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		_, err = cfg.defaultModelSelection(knownProviders)
		require.Error(t, err)
	})
	t.Run("should not error if model is missing", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "not-large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{}
		cfg.setDefaults("/tmp", "")
		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)
		_, err = cfg.defaultModelSelection(knownProviders)
		require.NoError(t, err)
	})

	t.Run("should configure the default models with a custom provider", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "$MISSING", // will not be included in the config
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "not-large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Models: []catwalk.Model{
						{
							ID:               "model",
							DefaultMaxTokens: 600,
						},
					},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")
		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)
		model, err := cfg.defaultModelSelection(knownProviders)
		require.NoError(t, err)
		require.Equal(t, "model", model.Model)
		require.Equal(t, "custom", model.Provider)
		require.Equal(t, int64(600), model.MaxTokens)
	})

	t.Run("should fail if no model configured", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "$MISSING", // will not be included in the config
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "not-large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Models:  []catwalk.Model{},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")
		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)
		_, err = cfg.defaultModelSelection(knownProviders)
		require.Error(t, err)
	})
	t.Run("should use the default provider first", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "set",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{
			Providers: csync.NewMap(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Models: []catwalk.Model{
						{
							ID:               "large-model",
							DefaultMaxTokens: 1000,
						},
					},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")
		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)
		model, err := cfg.defaultModelSelection(knownProviders)
		require.NoError(t, err)
		require.Equal(t, "large-model", model.Model)
		require.Equal(t, "openai", model.Provider)
		require.Equal(t, int64(1000), model.MaxTokens)
	})
}

func TestConfig_configureProvidersDisableDefaultProviders(t *testing.T) {
	t.Run("when enabled, ignores all default providers and requires full specification", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:          "openai",
				APIKey:      "$OPENAI_API_KEY",
				APIEndpoint: "https://api.openai.com/v1",
				Models: []catwalk.Model{{
					ID: "gpt-4",
				}},
			},
		}

		// User references openai but doesn't fully specify it (no base_url, no
		// models). This should be rejected because disable_default_providers
		// treats all providers as custom.
		cfg := &Config{
			Options: &Options{
				DisableDefaultProviders: true,
			},
			Providers: csync.NewMap(map[string]ProviderConfig{
				"openai": {
					APIKey: "$OPENAI_API_KEY",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{
			"OPENAI_API_KEY": "test-key",
		})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.ErrorContains(t, err, "no custom providers")

		// openai should NOT be present because it lacks base_url and models.
		require.Equal(t, 0, cfg.Providers.Len())
		_, exists := cfg.Providers.Get("openai")
		require.False(t, exists, "openai should not be present without full specification")
	})

	t.Run("when enabled, fully specified providers work", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:          "openai",
				APIKey:      "$OPENAI_API_KEY",
				APIEndpoint: "https://api.openai.com/v1",
				Models: []catwalk.Model{{
					ID: "gpt-4",
				}},
			},
		}

		// User fully specifies their provider.
		cfg := &Config{
			Options: &Options{
				DisableDefaultProviders: true,
			},
			Providers: csync.NewMap(map[string]ProviderConfig{
				"my-llm": {
					APIKey:  "$MY_API_KEY",
					BaseURL: "https://my-llm.example.com/v1",
					Models: []catwalk.Model{{
						ID: "my-model",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{
			"MY_API_KEY":     "test-key",
			"OPENAI_API_KEY": "test-key",
		})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		// Only fully specified provider should be present.
		require.Equal(t, 1, cfg.Providers.Len())
		provider, exists := cfg.Providers.Get("my-llm")
		require.True(t, exists, "my-llm should be present")
		require.Equal(t, "https://my-llm.example.com/v1", provider.BaseURL)
		require.Len(t, provider.Models, 1)

		// Default openai should NOT be present.
		_, exists = cfg.Providers.Get("openai")
		require.False(t, exists, "openai should not be present")
	})

	t.Run("when disabled, includes all known providers with valid credentials", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:          "openai",
				APIKey:      "$OPENAI_API_KEY",
				APIEndpoint: "https://api.openai.com/v1",
				Models: []catwalk.Model{{
					ID: "gpt-4",
				}},
			},
			{
				ID:          "anthropic",
				APIKey:      "$ANTHROPIC_API_KEY",
				APIEndpoint: "https://api.anthropic.com/v1",
				Models: []catwalk.Model{{
					ID: "claude-3",
				}},
			},
		}

		// User only configures openai, both API keys are available, but option
		// is disabled.
		cfg := &Config{
			Options: &Options{
				DisableDefaultProviders: false,
			},
			Providers: csync.NewMap(map[string]ProviderConfig{
				"openai": {
					APIKey: "$OPENAI_API_KEY",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{
			"OPENAI_API_KEY":    "test-key",
			"ANTHROPIC_API_KEY": "test-key",
		})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		// Both providers should be present.
		require.Equal(t, 2, cfg.Providers.Len())
		_, exists := cfg.Providers.Get("openai")
		require.True(t, exists, "openai should be present")
		_, exists = cfg.Providers.Get("anthropic")
		require.True(t, exists, "anthropic should be present")
	})

	t.Run("when enabled, provider missing models attempts discovery but still triggers no-custom-providers error", func(t *testing.T) {
		cfg := &Config{
			Options: &Options{
				DisableDefaultProviders: true,
			},
			Providers: csync.NewMap(map[string]ProviderConfig{
				"my-llm": {
					APIKey:  "test-key",
					BaseURL: "https://my-llm.example.com/v1",
					Models:  []catwalk.Model{}, // No models.
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.ErrorContains(t, err, "no custom providers")

		// Discovery fails (unreachable URL) so provider is removed.
		require.Equal(t, 0, cfg.Providers.Len())
	})

	t.Run("when enabled, provider missing base_url is rejected", func(t *testing.T) {
		cfg := &Config{
			Options: &Options{
				DisableDefaultProviders: true,
			},
			Providers: csync.NewMap(map[string]ProviderConfig{
				"my-llm": {
					APIKey: "test-key",
					Models: []catwalk.Model{{ID: "model"}},
					// No BaseURL.
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.ErrorContains(t, err, "no custom providers")

		// Provider should be rejected for missing base_url.
		require.Equal(t, 0, cfg.Providers.Len())
	})
}

func TestConfig_setDefaultsDisableDefaultProvidersEnvVar(t *testing.T) {
	t.Run("sets option from environment variable", func(t *testing.T) {
		t.Setenv("SENNIT_DISABLE_DEFAULT_PROVIDERS", "true")

		cfg := &Config{}
		cfg.setDefaults("/tmp", "")

		require.True(t, cfg.Options.DisableDefaultProviders)
	})

	t.Run("does not override when env var is not set", func(t *testing.T) {
		cfg := &Config{
			Options: &Options{
				DisableDefaultProviders: true,
			},
		}
		cfg.setDefaults("/tmp", "")

		require.True(t, cfg.Options.DisableDefaultProviders)
	})
}

func TestConfig_configureSelectedModels(t *testing.T) {
	t.Run("reload mode should not persist fallback defaults", func(t *testing.T) {
		dir := t.TempDir()
		globalPath := filepath.Join(dir, "sennit.json")
		require.NoError(t, os.WriteFile(globalPath, []byte(`{"model":{"provider":"ghost","model":"missing"}}`), 0o600))

		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				Models: []catwalk.Model{
					{ID: "large-model", DefaultMaxTokens: 1000},
				},
			},
		}

		cfg := &Config{
			Model: SelectedModel{Provider: "ghost", Model: "missing"},
		}
		cfg.setDefaults(dir, "")
		store := &ConfigStore{config: cfg, globalDataPath: globalPath}
		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), store, env, resolver, knownProviders)
		require.NoError(t, err)

		resolved, resolveErr := resolveSelectedModel(cfg, knownProviders)
		require.NoError(t, resolveErr)
		cfg.Model = resolved.Model

		// In-memory falls back to default.
		require.True(t, resolved.Fallback)
		require.Equal(t, "openai", cfg.Model.Provider)
		require.Equal(t, "large-model", cfg.Model.Model)

		// Disk remains unchanged (resolveSelectedModel never persists).
		data, readErr := os.ReadFile(globalPath)
		require.NoError(t, readErr)
		require.Contains(t, string(data), `"provider":"ghost"`)
		require.Contains(t, string(data), `"model":"missing"`)
	})
	t.Run("should override defaults", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				Models: []catwalk.Model{
					{
						ID:               "larger-model",
						DefaultMaxTokens: 2000,
					},
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
				},
			},
		}

		cfg := &Config{
			Model: SelectedModel{Model: "larger-model"},
		}
		cfg.setDefaults("/tmp", "")
		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		resolved, resolveErr := resolveSelectedModel(cfg, knownProviders)
		require.NoError(t, resolveErr)
		cfg.Model = resolved.Model
		require.Equal(t, "larger-model", cfg.Model.Model)
		require.Equal(t, "openai", cfg.Model.Provider)
		require.Equal(t, int64(2000), cfg.Model.MaxTokens)
	})
	t.Run("should be possible to select a model from a non-default provider", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				Models: []catwalk.Model{
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
				},
			},
			{
				ID:                  "anthropic",
				APIKey:              "abc",
				DefaultLargeModelID: "a-large-model",
				Models: []catwalk.Model{
					{
						ID:               "a-large-model",
						DefaultMaxTokens: 1000,
					},
				},
			},
		}

		cfg := &Config{
			Model: SelectedModel{
				Model:     "a-large-model",
				Provider:  "anthropic",
				MaxTokens: 300,
			},
		}
		cfg.setDefaults("/tmp", "")
		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		resolved, resolveErr := resolveSelectedModel(cfg, knownProviders)
		require.NoError(t, resolveErr)
		cfg.Model = resolved.Model
		require.Equal(t, "a-large-model", cfg.Model.Model)
		require.Equal(t, "anthropic", cfg.Model.Provider)
		require.Equal(t, int64(300), cfg.Model.MaxTokens)
	})

	t.Run("should override the max tokens only", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				Models: []catwalk.Model{
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
				},
			},
		}

		cfg := &Config{
			Model: SelectedModel{MaxTokens: 100},
		}
		cfg.setDefaults("/tmp", "")
		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		resolved, resolveErr := resolveSelectedModel(cfg, knownProviders)
		require.NoError(t, resolveErr)
		cfg.Model = resolved.Model
		require.Equal(t, "large-model", cfg.Model.Model)
		require.Equal(t, "openai", cfg.Model.Provider)
		require.Equal(t, int64(100), cfg.Model.MaxTokens)
	})
	t.Run("resolve and persist fallback under writeMu does not deadlock", func(t *testing.T) {
		dir := t.TempDir()
		globalPath := filepath.Join(dir, "sennit.json")
		require.NoError(t, os.WriteFile(globalPath, []byte(`{}`), 0o600))

		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				Models: []catwalk.Model{
					{ID: "large-model", DefaultMaxTokens: 1000},
				},
			},
		}

		cfg := &Config{
			Model: SelectedModel{Provider: "openai", Model: "this-model-does-not-exist"},
		}
		cfg.setDefaults(dir, "")
		store := &ConfigStore{config: cfg, globalDataPath: globalPath}
		env := testenv.New(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), store, env, resolver, knownProviders)
		require.NoError(t, err)

		// Simulate the Load path: resolve (pure), then persist the fallback
		// under writeMu using updateLocked. Before the refactor, the
		// combined configureSelectedModels(persist=true) self-deadlocked
		// because UpdatePreferredModel re-acquired writeMu.
		done := make(chan error, 1)
		go func() {
			resolved, resolveErr := resolveSelectedModel(cfg, knownProviders)
			if resolveErr != nil {
				done <- resolveErr
				return
			}
			cfg.Model = resolved.Model

			store.writeMu.Lock()
			defer store.writeMu.Unlock()
			if resolved.Fallback {
				if err := store.updateLocked(ScopeGlobal, func(c *Config) map[string]any {
					return store.updatePreferredModelFields(c, resolved.Model)
				}); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()

		select {
		case err := <-done:
			require.NoError(t, err)
			// Should have fallen back to the default.
			require.Equal(t, "large-model", cfg.Model.Model)
		case <-time.After(5 * time.Second):
			t.Fatal("resolve + persist deadlocked under writeMu")
		}
	})
}

func TestConfig_configureProviders_ProviderHeaderResolveError(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
		},
	}

	cfg := &Config{
		Providers: csync.NewMap(map[string]ProviderConfig{
			"openai": {
				ExtraHeaders: map[string]string{
					// Failing $(...) — inner command exits 1. Must
					// propagate as an error, not a silent truncation.
					"X-Broken": "$(false)",
				},
			},
		}),
	}
	cfg.setDefaults("/tmp", "")

	testEnv := testenv.New(map[string]string{
		"OPENAI_API_KEY": "test-key",
		"PATH":           os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, knownProviders)
	require.Error(t, err, "failing $(cmd) in a header must fail the provider load")
	require.Contains(t, err.Error(), "X-Broken", "error must name the offending header")
}

// TestConfig_configureProviders_CatwalkDefaultWithUnsetVarLoads
// verifies that a Catwalk-style default header like
// "OpenAI-Organization": "$OPENAI_ORG_ID" loads cleanly under lenient
// nounset (unset → "" → header dropped), and does not fail the load
// or leave the literal template on the wire.
func TestConfig_configureProviders_CatwalkDefaultWithUnsetVarLoads(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
			DefaultHeaders: map[string]string{
				"OpenAI-Organization": "$OPENAI_ORG_ID",
			},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")

	testEnv := testenv.New(map[string]string{
		"OPENAI_API_KEY": "test-key",
		"PATH":           os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, knownProviders)
	require.NoError(t, err, "optional env-gated header must not fail the load")

	pc, ok := cfg.Providers.Get("openai")
	require.True(t, ok, "openai provider must still be configured")
	_, present := pc.ExtraHeaders["OpenAI-Organization"]
	require.False(t, present, "header whose value resolves to empty must be absent")
}

// TestConfig_configureProviders_LiteralEmptyHeaderDropped pins design
// decision #18 for the literal case: a user-authored
// "X-Custom": "" in extra_headers is absent from the resolved map.
// Applies to both known- and custom-provider paths; this test
// exercises the custom-provider loop.
func TestConfig_configureProviders_LiteralEmptyHeaderDropped(t *testing.T) {
	cfg := &Config{
		Providers: csync.NewMap(map[string]ProviderConfig{
			"my-llm": {
				APIKey:  "test-key",
				BaseURL: "https://my-llm.example.com/v1",
				Type:    catwalk.TypeOpenAI,
				Models:  []catwalk.Model{{ID: "m"}},
				ExtraHeaders: map[string]string{
					"X-Custom": "",
					"X-Kept":   "present",
				},
			},
		}),
	}
	cfg.setDefaults("/tmp", "")

	testEnv := testenv.New(map[string]string{
		"PATH": os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, []catwalk.Provider{})
	require.NoError(t, err)

	pc, ok := cfg.Providers.Get("my-llm")
	require.True(t, ok)
	_, present := pc.ExtraHeaders["X-Custom"]
	require.False(t, present, "literal empty-string header must be dropped")
	require.Equal(t, "present", pc.ExtraHeaders["X-Kept"])
}

// TestConfig_configureProviders_EchoEmptyHeaderDropped pins design
// decision #18 for the non-failing empty case: $(echo) exits 0 with
// empty output, resolves cleanly to "", and must be dropped the same
// way an unset bare $VAR is. Exercises the known-provider loop.
func TestConfig_configureProviders_EchoEmptyHeaderDropped(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
			DefaultHeaders: map[string]string{
				"X-Empty": "$(echo)",
				"X-Kept":  "present",
			},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")

	testEnv := testenv.New(map[string]string{
		"OPENAI_API_KEY": "test-key",
		"PATH":           os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, knownProviders)
	require.NoError(t, err)

	pc, ok := cfg.Providers.Get("openai")
	require.True(t, ok)
	_, present := pc.ExtraHeaders["X-Empty"]
	require.False(t, present, "$(echo) → empty → header must be dropped")
	require.Equal(t, "present", pc.ExtraHeaders["X-Kept"])
}

// TestConfig_configureProviders_UnsetAPIKeySkipsProvider verifies that
// under the lenient-nounset shell resolver, $UNSET_API_KEY expands to
// ("", nil) rather than ("", err), and the existing
// `v == "" || err != nil` skip path at load.go:331 still drops the
// provider. The slog.Warn line is emitted on the same
// path but is not asserted here — internal/config/load_test.go's
// TestMain replaces the default slog handler with an io.Discard
// writer, so capturing that log line would require mid-test handler
// swapping and a sync.Mutex dance that adds more flake surface than
// signal. The observable outcome (provider absent from the map) is
// what downstream code — model picker, agent wiring — actually reads,
// so that's what we pin.
func TestConfig_configureProviders_UnsetAPIKeySkipsProvider(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$SOMETHING_UNSET",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
		},
	}

	// Existing user config for this known provider so the load.go:332
	// `if configExists` branch fires and actually calls Providers.Del.
	// Without it the provider was never in the map to begin with and
	// the test would pass trivially.
	cfg := &Config{
		Providers: csync.NewMap(map[string]ProviderConfig{
			"openai": {BaseURL: "custom-url"},
		}),
	}
	cfg.setDefaults("/tmp", "")

	testEnv := testenv.New(map[string]string{
		"PATH": os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, knownProviders)
	require.NoError(t, err, "skip path must not surface as a load error")

	require.Equal(t, 0, cfg.Providers.Len(), "provider with unset API key must be skipped")
	_, exists := cfg.Providers.Get("openai")
	require.False(t, exists)
}

// TestConfig_configureProviders_FailingAPIKeyCmdSkipsProvider pins
// that the two failure modes for APIKey — ("", nil) from an unset var
// under lenient nounset and ("", err) from a failing $(cmd) — are
// equivalent for the skip outcome at load.go:331. The `v == "" ||
// err != nil` check fires on either branch; this test locks in that
// equivalence so a future refactor that splits the check into two
// paths doesn't accidentally start propagating $(false) as a load
// error while keeping unset-var as a silent skip (or vice versa).
func TestConfig_configureProviders_FailingAPIKeyCmdSkipsProvider(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$(false)",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
		},
	}

	cfg := &Config{
		Providers: csync.NewMap(map[string]ProviderConfig{
			"openai": {BaseURL: "custom-url"},
		}),
	}
	cfg.setDefaults("/tmp", "")

	testEnv := testenv.New(map[string]string{
		"PATH": os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, knownProviders)
	require.NoError(t, err, "failing $(cmd) in API key must skip provider, not fail load")

	require.Equal(t, 0, cfg.Providers.Len(), "provider with failing $(cmd) API key must be skipped")
	_, exists := cfg.Providers.Get("openai")
	require.False(t, exists)
}

// TestConfig_configureProviders_UnsetAzureEndpointSkipsProvider pins
// the same contract on the Azure path at load.go:287 — APIEndpoint is
// the field that gates Azure and goes through the same
// `v == "" || err != nil` skip check. Covered here so both branches
// of the shared skip pattern (APIKey default path and APIEndpoint
// Azure path) are tested; a future refactor that unifies them can
// rely on these two tests to catch drift.
func TestConfig_configureProviders_UnsetAzureEndpointSkipsProvider(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          catwalk.InferenceProviderAzure,
			APIKey:      "test-key",
			APIEndpoint: "$UNSET_AZURE_ENDPOINT",
			Models:      []catwalk.Model{{ID: "test-model"}},
		},
	}

	cfg := &Config{
		Providers: csync.NewMap(map[string]ProviderConfig{
			"azure": {BaseURL: ""},
		}),
	}
	cfg.setDefaults("/tmp", "")

	testEnv := testenv.New(map[string]string{
		"PATH": os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, knownProviders)
	require.NoError(t, err)

	require.Equal(t, 0, cfg.Providers.Len(), "azure provider with unset endpoint must be skipped")
	_, exists := cfg.Providers.Get("azure")
	require.False(t, exists)
}

func TestConfig_LoadFromBytes_Env(t *testing.T) {
	data := []byte(`{"env": {"AWS_PROFILE": "my-profile", "AWS_REGION": "us-west-2"}}`)

	loadedConfig, err := loadFromBytes([][]byte{data})

	require.NoError(t, err)
	require.NotNil(t, loadedConfig.Env)
	require.Equal(t, "my-profile", loadedConfig.Env["AWS_PROFILE"])
	require.Equal(t, "us-west-2", loadedConfig.Env["AWS_REGION"])
}

func TestConfig_LoadFromBytes_EnvMerge(t *testing.T) {
	data1 := []byte(`{"env": {"AWS_PROFILE": "first", "AWS_REGION": "us-east-1"}}`)
	data2 := []byte(`{"env": {"AWS_PROFILE": "second"}}`)

	loadedConfig, err := loadFromBytes([][]byte{data1, data2})

	require.NoError(t, err)
	require.NotNil(t, loadedConfig.Env)
	require.Equal(t, "second", loadedConfig.Env["AWS_PROFILE"])
	require.Equal(t, "us-east-1", loadedConfig.Env["AWS_REGION"])
}

// TestGlobalLogFile verifies the log file lives alongside the shared
// database (both under GlobalDBDir), under SENNIT_GLOBAL_CONFIG so tests
// stay hermetic and never touch the real ~/.config/sennit.
func TestGlobalLogFile(t *testing.T) {
	globalDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)

	got := GlobalLogFile()

	require.Equal(t, filepath.Join(GlobalDBDir(), "logs", "sennit.log"), got)
	require.Equal(t, filepath.Join(globalDir, "logs", "sennit.log"), got)
}
