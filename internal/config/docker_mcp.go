package config

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

var dockerMCPVersionRunner = func(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "mcp", "version")
	return cmd.Run()
}

const dockerMCPAvailabilityTTL = 10 * time.Second

// dockerMCPCache holds Docker MCP availability behind an injectable clock,
// rather than the bare package-level struct measured against time.Since it
// used to be. See "Docker MCP availability is a process-global cache with a
// wall-clock TTL" in TECHDEBT.md: a single package-global cache makes every
// reader order-dependent across tests in the same process (whichever test
// warms the cache first decides what later tests observe), and a TTL
// measured against wall time lets a slow test run cross the 10s boundary
// mid-suite. now is swapped out in tests instead of sleeping past the TTL,
// and a test that needs isolation from others constructs its own instance
// rather than reaching for the package-level default.
type dockerMCPCache struct {
	mu        sync.Mutex
	available bool
	checkedAt time.Time
	known     bool

	ttl time.Duration
	now func() time.Time
}

func newDockerMCPCache() *dockerMCPCache {
	return &dockerMCPCache{ttl: dockerMCPAvailabilityTTL, now: time.Now}
}

// cached returns the cached availability and whether it is still fresh.
func (c *dockerMCPCache) cached() (available, known bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.known {
		return false, false
	}
	if c.now().Sub(c.checkedAt) > c.ttl {
		return c.available, false
	}
	return c.available, true
}

// set records a freshly checked availability, timestamped with c.now().
func (c *dockerMCPCache) set(available bool) {
	c.mu.Lock()
	c.available = available
	c.checkedAt = c.now()
	c.known = true
	c.mu.Unlock()
}

// defaultDockerMCPCache is the process-wide cache DockerMCPAvailabilityCached
// and RefreshDockerMCPAvailability go through. Tests that need isolation
// from other tests' cache state swap this out for a fresh instance rather
// than relying on the TTL to expire it; see swapDockerMCPCacheForTest in
// docker_mcp_test.go.
var defaultDockerMCPCache = newDockerMCPCache()

// DockerMCPName is the name of the Docker MCP configuration.
const DockerMCPName = "docker"

// IsDockerMCPAvailable checks if Docker MCP is available by running
// 'docker mcp version'.
func IsDockerMCPAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := dockerMCPVersionRunner(ctx)
	return err == nil
}

// DockerMCPAvailabilityCached returns the cached Docker MCP availability and
// whether the cached value is still fresh.
func DockerMCPAvailabilityCached() (available bool, known bool) {
	return defaultDockerMCPCache.cached()
}

// RefreshDockerMCPAvailability refreshes and caches Docker MCP availability.
func RefreshDockerMCPAvailability() bool {
	available := IsDockerMCPAvailable()
	defaultDockerMCPCache.set(available)
	return available
}

// IsDockerMCPEnabled checks if Docker MCP is already configured.
func (c *Config) IsDockerMCPEnabled() bool {
	if c.MCP == nil {
		return false
	}
	_, exists := c.MCP[DockerMCPName]
	return exists
}

// DockerMCPConfig returns the default Docker MCP stdio configuration.
func DockerMCPConfig() MCPConfig {
	return MCPConfig{
		Type:     MCPStdio,
		Command:  "docker",
		Args:     []string{"mcp", "gateway", "run"},
		Disabled: false,
	}
}

// PrepareDockerMCPConfig validates Docker MCP availability and stages the
// Docker MCP configuration in memory.
func (s *ConfigStore) PrepareDockerMCPConfig() (MCPConfig, error) {
	if !IsDockerMCPAvailable() {
		return MCPConfig{}, fmt.Errorf("docker mcp is not available, please ensure docker is installed and 'docker mcp version' succeeds")
	}

	mcpConfig := DockerMCPConfig()
	// In-memory only; persistence happens in PersistDockerMCPConfig.
	s.mutateInMemory(func(c *Config) {
		if c.MCP == nil {
			c.MCP = make(map[string]MCPConfig)
		}
		c.MCP[DockerMCPName] = mcpConfig
	})
	return mcpConfig, nil
}

// PersistDockerMCPConfig persists a previously prepared Docker MCP
// configuration to the global config file.
func (s *ConfigStore) PersistDockerMCPConfig(mcpConfig MCPConfig) error {
	if err := s.SetConfigField(ScopeGlobal, "mcp."+DockerMCPName, mcpConfig); err != nil {
		return fmt.Errorf("failed to persist docker mcp configuration: %w", err)
	}
	return nil
}

// EnableDockerMCP adds Docker MCP configuration and persists it.
func (s *ConfigStore) EnableDockerMCP() error {
	mcpConfig, err := s.PrepareDockerMCPConfig()
	if err != nil {
		return err
	}
	if err := s.PersistDockerMCPConfig(mcpConfig); err != nil {
		return err
	}
	return nil
}

// DisableDockerMCP removes Docker MCP configuration and persists the change.
func (s *ConfigStore) DisableDockerMCP() error {
	return s.update(ScopeGlobal, func(c *Config) map[string]any {
		if c.MCP == nil {
			return nil
		}
		delete(c.MCP, DockerMCPName)
		return map[string]any{"mcp": c.MCP}
	})
}

// RemoveDockerMCPInMemory removes the Docker MCP entry from the in-memory
// config via copy-on-write, without persisting to disk. It rolls back a
// staged PrepareDockerMCPConfig when starting or persisting the server
// fails, so callers must not mutate Config().MCP directly.
func (s *ConfigStore) RemoveDockerMCPInMemory() {
	s.mutateInMemory(func(c *Config) {
		delete(c.MCP, DockerMCPName)
	})
}
