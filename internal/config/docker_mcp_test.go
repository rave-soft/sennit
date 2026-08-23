package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/env"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

var errDockerUnavailable = errors.New("docker unavailable")

func setDockerMCPVersionRunner(t *testing.T, runner func(context.Context) error) {
	t.Helper()
	orig := dockerMCPVersionRunner
	dockerMCPVersionRunner = runner
	t.Cleanup(func() {
		dockerMCPVersionRunner = orig
	})
}

// swapDockerMCPCacheForTest installs a fresh dockerMCPCache as
// defaultDockerMCPCache for the duration of the test, so this test cannot
// observe (or leave behind for a sibling test to observe) whatever another
// test warmed the shared cache to. This is the fix for the "process-global
// cache" half of the TECHDEBT.md entry: previously every test reading
// DockerMCPAvailabilityCached shared one package-level cache with no reset
// seam.
func swapDockerMCPCacheForTest(t *testing.T) *dockerMCPCache {
	t.Helper()
	orig := defaultDockerMCPCache
	fresh := newDockerMCPCache()
	defaultDockerMCPCache = fresh
	t.Cleanup(func() {
		defaultDockerMCPCache = orig
	})
	return fresh
}

func TestIsDockerMCPEnabled(t *testing.T) {
	t.Parallel()

	t.Run("returns false when MCP is nil", func(t *testing.T) {
		t.Parallel()
		cfg := &Config{
			MCP: nil,
		}
		require.False(t, cfg.IsDockerMCPEnabled())
	})

	t.Run("returns false when docker mcp not configured", func(t *testing.T) {
		t.Parallel()
		cfg := &Config{
			MCP: make(map[string]MCPConfig),
		}
		require.False(t, cfg.IsDockerMCPEnabled())
	})

	t.Run("returns true when docker mcp is configured", func(t *testing.T) {
		t.Parallel()
		cfg := &Config{
			MCP: map[string]MCPConfig{
				DockerMCPName: {
					Type:    MCPStdio,
					Command: "docker",
				},
			},
		}
		require.True(t, cfg.IsDockerMCPEnabled())
	})
}

func TestEnableDockerMCP(t *testing.T) {
	t.Run("adds docker mcp to config", func(t *testing.T) {
		setDockerMCPVersionRunner(t, func(context.Context) error { return nil })

		// Create a temporary directory for config.
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "sennit.json")

		cfg := &Config{
			MCP: make(map[string]MCPConfig),
		}
		store := &ConfigStore{
			config:         cfg,
			globalDataPath: configPath,
			resolver:       NewShellVariableResolver(env.New()),
		}

		err := store.EnableDockerMCP()
		require.NoError(t, err)

		// Check in-memory config via the store (copy-on-write publishes
		// a new Config; the captured cfg pointer stays unchanged).
		require.True(t, store.Config().IsDockerMCPEnabled())
		mcpConfig, exists := store.Config().MCP[DockerMCPName]
		require.True(t, exists)
		require.Equal(t, MCPStdio, mcpConfig.Type)
		require.Equal(t, "docker", mcpConfig.Command)
		require.Equal(t, []string{"mcp", "gateway", "run"}, mcpConfig.Args)
		require.False(t, mcpConfig.Disabled)

		// Check persisted config.
		data, err := os.ReadFile(configPath)
		require.NoError(t, err)
		require.Contains(t, string(data), "docker")
		require.Contains(t, string(data), "gateway")
	})

	t.Run("fails when docker mcp not available", func(t *testing.T) {
		setDockerMCPVersionRunner(t, func(context.Context) error { return errDockerUnavailable })

		// Create a temporary directory for config.
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "sennit.json")

		cfg := &Config{
			MCP: make(map[string]MCPConfig),
		}
		store := &ConfigStore{
			config:         cfg,
			globalDataPath: configPath,
			resolver:       NewShellVariableResolver(env.New()),
		}

		err := store.EnableDockerMCP()
		require.Error(t, err)
		require.Contains(t, err.Error(), "docker mcp is not available")
	})
}

func TestDisableDockerMCP(t *testing.T) {
	t.Parallel()

	t.Run("removes docker mcp from config", func(t *testing.T) {
		t.Parallel()

		// Create a temporary directory for config.
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "sennit.json")

		cfg := &Config{
			MCP: map[string]MCPConfig{
				DockerMCPName: {
					Type:     MCPStdio,
					Command:  "docker",
					Args:     []string{"mcp", "gateway", "run"},
					Disabled: false,
				},
			},
		}
		store := &ConfigStore{
			config:         cfg,
			globalDataPath: configPath,
			resolver:       NewShellVariableResolver(env.New()),
		}

		// Verify it's enabled first.
		require.True(t, cfg.IsDockerMCPEnabled())

		err := store.DisableDockerMCP()
		require.NoError(t, err)

		// Check in-memory config via the store (copy-on-write publishes a
		// new Config; the original cfg pointer is intentionally unchanged).
		require.False(t, store.Config().IsDockerMCPEnabled())
		_, exists := store.Config().MCP[DockerMCPName]
		require.False(t, exists)
	})

	t.Run("does nothing when MCP is nil", func(t *testing.T) {
		t.Parallel()

		cfg := &Config{
			MCP: nil,
		}
		store := &ConfigStore{
			config:         cfg,
			globalDataPath: filepath.Join(t.TempDir(), "sennit.json"),
			resolver:       NewShellVariableResolver(env.New()),
		}

		err := store.DisableDockerMCP()
		require.NoError(t, err)
	})

	// TestDisableDockerMCP/does_not_leak_project-scoped_servers_into_global_config
	// guards against writing the whole in-memory c.MCP map (the merge of
	// every config layer) back to the global file: c.MCP can hold a
	// project-scoped server -- with its oauth_token -- that the global
	// file never declared, and DisableDockerMCP must only ever touch the
	// single "mcp.docker" field, never copy that server (or its token)
	// into the global file.
	t.Run("does not leak project-scoped servers into global config", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "sennit.json")
		globalContent := `{"mcp":{"docker":{"type":"stdio","command":"docker","args":["mcp","gateway","run"]}}}`
		require.NoError(t, os.WriteFile(configPath, []byte(globalContent), 0o600))

		// c.MCP is the already-merged map: it also carries a
		// project-scoped server, declared only in some project's own
		// config, complete with a secret oauth_token that must never end
		// up in the global file.
		cfg := &Config{
			MCP: map[string]MCPConfig{
				DockerMCPName: {
					Type:    MCPStdio,
					Command: "docker",
					Args:    []string{"mcp", "gateway", "run"},
				},
				"project-server": {
					Type:       MCPStdio,
					Command:    "project-tool",
					OAuthToken: &oauth.Token{AccessToken: "super-secret"},
				},
			},
		}
		store := &ConfigStore{
			config:         cfg,
			globalDataPath: configPath,
			resolver:       NewShellVariableResolver(env.New()),
		}

		err := store.DisableDockerMCP()
		require.NoError(t, err)

		data, err := os.ReadFile(configPath)
		require.NoError(t, err)
		require.False(t, gjson.GetBytes(data, "mcp.docker").Exists(), "mcp.docker should be removed")
		require.False(t, gjson.GetBytes(data, "mcp.project-server").Exists(),
			"a project-scoped server must never be written into the global config")
		require.NotContains(t, string(data), "super-secret",
			"a project-scoped server's oauth_token must never leak into the global config")
	})
}

// TestDockerMCPCache_TTLUsesInjectedClockNotWallTime pins the fix for
// "Docker MCP availability is a process-global cache with a wall-clock TTL"
// (TECHDEBT.md): the cache's freshness check now goes through an injected
// clock instead of time.Since, so a test can drive it past its TTL
// deterministically instead of sleeping (or risking a slow run crossing the
// boundary by accident).
func TestDockerMCPCache_TTLUsesInjectedClockNotWallTime(t *testing.T) {
	now := time.Now()
	c := newDockerMCPCache()
	c.now = func() time.Time { return now }

	c.set(true)
	available, known := c.cached()
	require.True(t, known, "freshly set value must be known")
	require.True(t, available)

	// Advance the injected clock past the TTL without any real time
	// passing or any t.Sleep.
	now = now.Add(dockerMCPAvailabilityTTL + time.Millisecond)
	available, known = c.cached()
	require.False(t, known, "a value past its TTL must be reported stale")
	require.True(t, available, "the stale value itself is still returned, just marked unknown")
}

// TestDockerMCPCache_IsolatedAcrossInstances pins the other half of the same
// fix: whatever one test's cache instance was warmed to must not leak into
// another's. Before the fix there was exactly one package-level cache, so
// whichever test in the process ran first decided what every later test
// calling DockerMCPAvailabilityCached observed.
func TestDockerMCPCache_IsolatedAcrossInstances(t *testing.T) {
	first := newDockerMCPCache()
	first.set(true)

	second := newDockerMCPCache()
	_, known := second.cached()
	require.False(t, known, "a fresh cache instance must not see another instance's state")
}

// TestDockerMCPAvailabilityCached_IsolatedBetweenTests exercises the actual
// package-level entry points (DockerMCPAvailabilityCached,
// RefreshDockerMCPAvailability) rather than dockerMCPCache directly, via the
// swap seam a test uses for isolation.
func TestDockerMCPAvailabilityCached_IsolatedBetweenTests(t *testing.T) {
	swapDockerMCPCacheForTest(t)
	setDockerMCPVersionRunner(t, func(context.Context) error { return nil })

	_, known := DockerMCPAvailabilityCached()
	require.False(t, known, "a swapped-in fresh cache must start unknown")

	require.True(t, RefreshDockerMCPAvailability())
	available, known := DockerMCPAvailabilityCached()
	require.True(t, known)
	require.True(t, available)
}

func TestEnableDockerMCPWithRealDockerWhenAvailable(t *testing.T) {
	t.Parallel()

	if !IsDockerMCPAvailable() {
		t.Skip("docker mcp not available on this machine")
	}

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "sennit.json")

	cfg := &Config{
		MCP: make(map[string]MCPConfig),
	}
	store := &ConfigStore{
		config:         cfg,
		globalDataPath: configPath,
		resolver:       NewShellVariableResolver(env.New()),
	}

	err := store.EnableDockerMCP()
	require.NoError(t, err)
	require.True(t, store.Config().IsDockerMCPEnabled())
}
