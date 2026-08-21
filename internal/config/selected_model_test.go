package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/testenv"
	"github.com/stretchr/testify/require"
)

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
		err := cfg.configureProviders(context.Background(), NewTestStore(t, cfg), env, resolver, knownProviders)
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
		err := cfg.configureProviders(context.Background(), NewTestStore(t, cfg), env, resolver, knownProviders)
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
		err := cfg.configureProviders(context.Background(), NewTestStore(t, cfg), env, resolver, knownProviders)
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
		err := cfg.configureProviders(context.Background(), NewTestStore(t, cfg), env, resolver, knownProviders)
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
		err := cfg.configureProviders(context.Background(), NewTestStore(t, cfg), env, resolver, knownProviders)
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
		err := cfg.configureProviders(context.Background(), NewTestStore(t, cfg), env, resolver, knownProviders)
		require.NoError(t, err)
		model, err := cfg.defaultModelSelection(knownProviders)
		require.NoError(t, err)
		require.Equal(t, "large-model", model.Model)
		require.Equal(t, "openai", model.Provider)
		require.Equal(t, int64(1000), model.MaxTokens)
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
		err := cfg.configureProviders(context.Background(), NewTestStore(t, cfg), env, resolver, knownProviders)
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
		err := cfg.configureProviders(context.Background(), NewTestStore(t, cfg), env, resolver, knownProviders)
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
		err := cfg.configureProviders(context.Background(), NewTestStore(t, cfg), env, resolver, knownProviders)
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
