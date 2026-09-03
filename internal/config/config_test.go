package config

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/stretchr/testify/require"
)

func TestDefaultModelForProvider(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:                  "anthropic",
			DefaultLargeModelID: "claude-3-opus",
		},
	}

	newConfig := func(providers map[string]ProviderConfig) *Config {
		m := csync.NewMap[string, ProviderConfig]()
		for id, p := range providers {
			m.Set(id, p)
		}
		return &Config{Providers: m}
	}

	t.Run("catalog provider uses DefaultLargeModelID when configured", func(t *testing.T) {
		cfg := newConfig(map[string]ProviderConfig{
			"anthropic": {
				ID: "anthropic",
				Models: []catwalk.Model{
					{ID: "claude-3-haiku", DefaultMaxTokens: 4096},
					{ID: "claude-3-opus", DefaultMaxTokens: 8192, DefaultReasoningEffort: "high"},
				},
			},
		})

		got, err := cfg.DefaultModelForProvider("anthropic", knownProviders)
		require.NoError(t, err)
		require.Equal(t, SelectedModel{
			Provider:        "anthropic",
			Model:           "claude-3-opus",
			MaxTokens:       8192,
			ReasoningEffort: "high",
		}, got)
	})

	t.Run("falls back to first configured model when default not present", func(t *testing.T) {
		cfg := newConfig(map[string]ProviderConfig{
			"anthropic": {
				ID: "anthropic",
				Models: []catwalk.Model{
					{ID: "claude-3-haiku", DefaultMaxTokens: 4096},
				},
			},
		})

		got, err := cfg.DefaultModelForProvider("anthropic", knownProviders)
		require.NoError(t, err)
		require.Equal(t, SelectedModel{
			Provider:  "anthropic",
			Model:     "claude-3-haiku",
			MaxTokens: 4096,
		}, got)
	})

	t.Run("falls back to first configured model for provider outside the catalog", func(t *testing.T) {
		cfg := newConfig(map[string]ProviderConfig{
			"my-custom-provider": {
				ID: "my-custom-provider",
				Models: []catwalk.Model{
					{ID: "custom-model-a", DefaultMaxTokens: 2048},
					{ID: "custom-model-b", DefaultMaxTokens: 4096},
				},
			},
		})

		got, err := cfg.DefaultModelForProvider("my-custom-provider", knownProviders)
		require.NoError(t, err)
		require.Equal(t, SelectedModel{
			Provider:  "my-custom-provider",
			Model:     "custom-model-a",
			MaxTokens: 2048,
		}, got)
	})

	t.Run("errors when the provider is not configured", func(t *testing.T) {
		cfg := newConfig(map[string]ProviderConfig{})

		_, err := cfg.DefaultModelForProvider("anthropic", knownProviders)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not configured")
	})

	t.Run("errors when the configured provider has no models", func(t *testing.T) {
		cfg := newConfig(map[string]ProviderConfig{
			"anthropic": {ID: "anthropic"},
		})

		_, err := cfg.DefaultModelForProvider("anthropic", knownProviders)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no models configured")
	})
}

// TestAgentOverride_NilConfig verifies AgentOverride is nil-safe like its
// sibling accessors on *Config, rather than panicking on a nil receiver
// (e.g. a test double that never got a published Config).
func TestAgentOverride_NilConfig(t *testing.T) {
	var cfg *Config
	model, effort, ok := cfg.AgentOverride("some-agent")
	require.False(t, ok)
	require.Empty(t, model)
	require.Empty(t, effort)
}
