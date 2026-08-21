package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/stretchr/testify/require"
)

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
