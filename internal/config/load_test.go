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
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rave-soft/sennit/internal/config/migrate"
	"github.com/rave-soft/sennit/internal/config/modelcache"
	"github.com/rave-soft/sennit/internal/shellconfig"
	"github.com/rave-soft/sennit/internal/testenv"
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

func TestConfig_LoadFromBytes_Tombstones(t *testing.T) {
	marker := func(section, name string) []byte {
		return []byte(`{"` + section + `":{"` + name + `":{"` + shellconfig.TombstoneKey + `":{"section":"` + section + `","name":"` + name + `"}}}}`)
	}

	t.Run("higher shell layer replaces a removed MCP entry without inheritance", func(t *testing.T) {
		lower := []byte(`{"mcp":{"server":{"type":"http","command":"old","args":["old"],"env":{"OLD":"1"},"url":"https://old.test","headers":{"Old":"value"},"oauth":true,"oauth_client_id":"old-client","timeout":99,"disabled":true}}}`)
		path := filepath.Join(t.TempDir(), "sennitrc")
		fresh, err := shellconfig.LoadShellConfig(t.Context(), path, []byte("mcp remove server\nmcp add server --command new"))
		require.NoError(t, err)
		cfg, err := loadFromBytes([][]byte{lower, fresh})
		require.NoError(t, err)
		server := cfg.MCP["server"]
		require.Equal(t, MCPStdio, server.Type)
		require.Equal(t, "new", server.Command)
		require.Empty(t, server.Args)
		require.Empty(t, server.Env)
		require.Empty(t, server.URL)
		require.Empty(t, server.Headers)
		require.False(t, server.OAuth)
		require.Empty(t, server.OAuthClientID)
		require.Zero(t, server.Timeout)
		require.False(t, server.Disabled)
	})

	t.Run("shell layers remove lower MCP and LSP entries", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sennitrc")
		lower, err := shellconfig.LoadShellConfig(t.Context(), path, []byte("mcp add server --command node --args old --env OLD 1\nlsp add language --command gopls"))
		require.NoError(t, err)
		higher, err := shellconfig.LoadShellConfig(t.Context(), path, []byte("mcp remove server\nlsp remove language"))
		require.NoError(t, err)
		cfg, err := loadFromBytes([][]byte{lower, higher})
		require.NoError(t, err)
		require.NotContains(t, cfg.MCP, "server")
		require.NotContains(t, cfg.LSP, "language")
	})

	t.Run("MCP and LSP tombstones remain section-scoped", func(t *testing.T) {
		lower := []byte(`{"mcp":{"server":{"type":"stdio","command":"node"}},"lsp":{"server":{"command":"gopls"}}}`)
		cfg, err := loadFromBytes([][]byte{lower, marker("mcp", "server")})
		require.NoError(t, err)
		require.NotContains(t, cfg.MCP, "server")
		require.Equal(t, "gopls", cfg.LSP["server"].Command)
	})

	t.Run("malformed markers cannot silently delete entries", func(t *testing.T) {
		lower := []byte(`{"mcp":{"server":{"type":"stdio","command":"node"}}}`)
		malformed := []byte(`{"mcp":{"server":{"` + shellconfig.TombstoneKey + `":{"section":"mcp","name":"server"},"command":"bad"}}}`)
		_, err := loadFromBytes([][]byte{lower, malformed})
		require.Error(t, err)
	})
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
		if runtime.GOOS == "windows" {
			// The provocation below removes workspaceDir and puts a plain
			// file there instead, so opening workspaceDir/sennit.json fails
			// because its parent isn't a directory. On Unix that's ENOTDIR,
			// which os.IsNotExist rejects, so applyWorkspaceConfig correctly
			// surfaces it as a real read error — the property this subtest
			// exists to cover. On Windows the same setup reports
			// ERROR_PATH_NOT_FOUND, which Go's syscall.Errno.Is maps to
			// fs.ErrNotExist, so the read is indistinguishable from "no
			// config here" and applyWorkspaceConfig (correctly) treats it as
			// a no-op. There is no directory-vs-file trick that provokes a
			// genuine non-not-exist read error identically on both
			// platforms, so this subtest only runs on Unix.
			t.Skip("parent-not-a-directory reads as ERROR_PATH_NOT_FOUND (fs.ErrNotExist) on Windows, not a real read error")
		}

		cfg := newConfig()
		var loaded []string
		require.NoError(t, os.RemoveAll(workspaceDir))
		require.NoError(t, os.WriteFile(workspaceDir, nil, 0o644))
		// Registered before the assertions below run, so a failed
		// require.Error still restores workspaceDir to a directory for the
		// subtests that follow — otherwise their os.MkdirAll(workspaceDir)
		// fails against the leftover file forever.
		t.Cleanup(func() {
			_ = os.Remove(workspaceDir)
		})

		err := applyWorkspaceConfig(cfg, workingDir, &loaded)
		require.Error(t, err)
		require.Contains(t, err.Error(), workspacePath)
		var pathErr *os.PathError
		require.True(t, errors.As(err, &pathErr))
		require.Equal(t, "open", pathErr.Op)
		require.Equal(t, workspacePath, pathErr.Path)
		require.Error(t, pathErr.Err)
		require.Empty(t, loaded)
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
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, appName+".json"), []byte(`{"agents":{},"options":{"data_directory":`+jsonPath(t, workspaceDir)+`}}`), 0o644))

	workspacePath := filepath.Join(workspaceDir, appName+".json")
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
	require.NoError(t, os.WriteFile(workspacePath, []byte(`{"options":{"debug":true}}`), 0o644))

	store, err := loadRuntimeForTest(workingDir, "", false)
	require.NoError(t, err)
	require.True(t, store.Config().jsonAgentsBlockDetected)
}

func TestLoad_WorkspaceLegacyRecentModelsPreservesSiblingFields(t *testing.T) {
	workingDir := t.TempDir()
	globalDir := t.TempDir()
	workspaceDir := filepath.Join(t.TempDir(), "custom-workspace-data")
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDir)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, appName+".json"), []byte(`{"options":{"data_directory":`+jsonPath(t, workspaceDir)+`}}`), 0o644))

	workspacePath := filepath.Join(workspaceDir, appName+".json")
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
	require.NoError(t, os.WriteFile(workspacePath, []byte(`{"recent_models":{"large":[]},"options":{"debug":true}}`), 0o644))
	require.NoError(t, Trust(workingDir))

	store, err := loadRuntimeForTest(workingDir, "", false)
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

	cfg, loaded, err := loadFromConfigPaths(context.Background(), []string{path}, true)

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

		cfg, _, err := loadFromConfigPaths(context.Background(), lookupConfigs(project), true)
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

		cfg, _, err := loadFromConfigPaths(context.Background(), lookupConfigs(project), true)
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

		_, _, err := loadFromConfigPaths(context.Background(), []string{good, bad}, true)
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
		}, true)
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

		_, _, err := loadFromConfigPaths(context.Background(), []string{jsonPath, rcPath}, true)
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

		_, _, err := loadFromConfigPaths(context.Background(), []string{jsonPath, rcPath}, true)
		require.NoError(t, err)
		require.NotContains(t, buf.String(), "sennitrc taking precedence",
			"disjoint coexistence should not warn")
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
	store1, err := loadRuntimeForTest(workingDir1, "", false)
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
	cached, ok := modelcache.New(dataConfigPath).Load("custom")
	require.True(t, ok)
	require.Len(t, cached, 1)
	require.Equal(t, "auto-model", cached[0].ID)

	// A second, independent Load of the same data dir must find the
	// models already populated (from the cache) and skip discovery
	// entirely.
	workingDir2 := t.TempDir()
	store2, err := loadRuntimeForTest(workingDir2, "", false)
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
	store, err := loadRuntimeForTest(workingDir, "", false)
	require.NoError(t, err)
	_, exists := store.config.Providers.Get("custom")
	require.False(t, exists, "provider with failed discovery and no models must be dropped")

	after, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "failed discovery must not write to disk")
	require.False(t, gjson.GetBytes(after, "providers.custom.models").Exists())
}

// TestConfig_Load_ProjectProvidersIgnored verifies that a project-level
// sennit.json cannot contribute provider (or model) settings: providers are
// global-only, so the project's models list for the same provider ID is
// dropped before the merge and the data-dir config's entry survives
// untouched. Discovery must still be short-circuited by that non-empty list,
// so no HTTP request is made. The stale data-dir models list is migrated out
// of the JSON file and into the model cache as a side effect of Load (see
// migrateBloatedModelCache); this test does not cover that migration's own
// bookkeeping (see TestConfig_Load_MigratesBloatedModelCache).
func TestConfig_Load_ProjectProvidersIgnored(t *testing.T) {
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
	// list for the same provider ID. It must be ignored in full.
	workingDir := t.TempDir()
	projectSeed := `{"providers": {"custom": {"models": [{"id": "project-model", "name": "project-model"}]}}}`
	projectPath := filepath.Join(workingDir, "sennit.json")
	require.NoError(t, os.WriteFile(projectPath, []byte(projectSeed), 0o644))
	require.NoError(t, Trust(workingDir))

	store, err := loadRuntimeForTest(workingDir, "", false)
	require.NoError(t, err)
	pc, ok := store.config.Providers.Get("custom")
	require.True(t, ok)

	require.Len(t, pc.Models, 1)
	require.Equal(t, "stale-model", pc.Models[0].ID)
	require.Equal(t, server.URL+"/v1", pc.BaseURL)

	// A non-empty models list must short-circuit discovery.
	require.Equal(t, int64(0), requests.Load(), "discovery must not run when models is already non-empty")

	// The ignore is reported rather than silent.
	require.True(t, slices.ContainsFunc(Doctor(store.Config()), func(p Problem) bool {
		return p.Area == AreaProvider && p.Subject == projectPath
	}), "the ignored project providers block must show up in the doctor")
}

func TestConfig_LoadFromBytes_LayeredTombstones(t *testing.T) {
	marker := func(section, name string) []byte {
		return []byte(fmt.Sprintf(`{"%s":{"%s":{"%s":{"section":"%s","name":"%s"}}}}`, section, name, shellconfig.TombstoneKey, section, name))
	}

	t.Run("MCP deletion survives token overlay and upper declaration restores fresh entry", func(t *testing.T) {
		lower := []byte(`{"mcp":{"server":{"type":"stdio","command":"old","args":["old"],"env":{"OLD":"1"}}}}`)
		token := []byte(`{"mcp":{"server":{"oauth_token":{"access_token":"ignored"}}}}`)
		upper := []byte(`{"mcp":{"server":{"type":"stdio","command":"new"}}}`)

		removed, err := loadFromBytes([][]byte{lower, marker("mcp", "server"), token})
		require.NoError(t, err)
		require.NotContains(t, removed.MCP, "server")

		restored, err := loadFromBytes([][]byte{lower, marker("mcp", "server"), token, upper})
		require.NoError(t, err)
		require.Equal(t, "new", restored.MCP["server"].Command)
		require.Empty(t, restored.MCP["server"].Args)
		require.Empty(t, restored.MCP["server"].Env)
	})

	t.Run("LSP deletion is section scoped", func(t *testing.T) {
		lower := []byte(`{"mcp":{"gopls":{"type":"stdio","command":"mcp"}},"lsp":{"gopls":{"command":"gopls","args":["serve"],"env":{"OLD":"1"}}}}`)
		cfg, err := loadFromBytes([][]byte{lower, marker("lsp", "gopls")})
		require.NoError(t, err)
		require.NotContains(t, cfg.LSP, "gopls")
		require.Contains(t, cfg.MCP, "gopls")
	})

	t.Run("Provider deletion is section scoped and masks a lower layer", func(t *testing.T) {
		lower := []byte(`{"providers":{"dropme":{"api_key":"k","base_url":"http://x"}},"mcp":{"dropme":{"type":"stdio","command":"mcp"}}}`)
		cfg, err := loadFromBytes([][]byte{lower, marker("providers", "dropme")})
		require.NoError(t, err)
		_, ok := cfg.Providers.Get("dropme")
		require.False(t, ok, "provider removed in a higher layer must not survive the merge")
		require.Contains(t, cfg.MCP, "dropme", "the tombstone must stay scoped to providers")
	})

	t.Run("Provider tombstone marker never leaks into the merged config", func(t *testing.T) {
		lower := []byte(`{"providers":{"dropme":{"api_key":"k","base_url":"http://x"}}}`)
		cfg, err := loadFromBytes([][]byte{lower, marker("providers", "dropme")})
		require.NoError(t, err)
		require.Zero(t, cfg.Providers.Len(), "the tombstoned entry must not linger as a provider named __sennit_tombstone or dropme")
		for id := range cfg.Providers.Seq2() {
			require.NotEqual(t, shellconfig.TombstoneKey, id)
			require.NotEqual(t, "dropme", id)
		}
	})

	t.Run("Malformed and misplaced markers fail or remain ordinary data", func(t *testing.T) {
		_, err := loadFromBytes([][]byte{[]byte(`{"mcp":{"server":{"__sennit_tombstone":{"section":"mcp","name":"server"},"command":"bad"}}}`)})
		require.ErrorContains(t, err, "must not contain other fields")

		cfg, err := loadFromBytes([][]byte{[]byte(`{"options":{"tombstone":{"__sennit_tombstone":{"section":"mcp","name":"server"}}}}`)})
		require.NoError(t, err)
		require.NotNil(t, cfg.Options)
	})
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
