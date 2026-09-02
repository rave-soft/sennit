package config

import (
	"fmt"

	"github.com/rave-soft/sennit/internal/dockermcp"
	"github.com/rave-soft/sennit/internal/mcpid"
)

// DockerMCPName is the name of the Docker MCP configuration. It is defined
// in internal/mcpid so the UI can reference it without importing config.
const DockerMCPName = mcpid.DockerMCPName

// IsDockerMCPEnabled checks if Docker MCP is already configured.
func (c *Config) IsDockerMCPEnabled() bool {
	if c.MCP == nil {
		return false
	}
	_, exists := c.MCP[DockerMCPName]
	return exists
}

// DockerMCPConfig returns the default Docker MCP stdio configuration. It
// stays here, rather than in internal/dockermcp, because it is config-
// shaped data (an MCPConfig) rather than availability detection; keeping it
// here also keeps the dependency arrow pointing from config to dockermcp,
// never back.
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
	if !dockermcp.IsAvailable() {
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

// DisableDockerMCP removes Docker MCP configuration and persists the
// change.
//
// This must only delete the single "mcp.docker" field, not rewrite the
// whole (already-merged) c.MCP map back to disk: c.MCP is the merge of
// every config layer, so writing it whole into the global file would copy
// project-scoped servers -- including their oauth_token -- into the global
// config and leak them into every other project.
//
// The in-memory removal is applied directly rather than left to
// RemoveConfigField's autoReload, so the change is visible immediately even
// when workingDir isn't set up for a full disk reload (e.g. in tests that
// construct a bare ConfigStore).
func (s *ConfigStore) DisableDockerMCP() error {
	s.mutateInMemory(func(c *Config) {
		delete(c.MCP, DockerMCPName)
	})
	return s.RemoveConfigField(ScopeGlobal, "mcp."+DockerMCPName)
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
