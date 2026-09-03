package providerload

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/stretchr/testify/require"
)

type recordingStore struct {
	removed []string
}

func (store *recordingStore) RemoveRuntimeConfigField(_ config.Scope, key string) {
	store.removed = append(store.removed, key)
}

func TestLoadRuntimeResolverReadsCurrentEnvironment(t *testing.T) {
	workingDir := t.TempDir()
	globalDir := t.TempDir()
	workspaceDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDir)
	t.Setenv("PROVIDERLOAD_LATE_PROCESS", "before-load")
	t.Setenv("SENNIT_PROVIDERLOAD_OVERRIDE", "override")

	globalConfig, err := json.Marshal(map[string]any{
		"options": map[string]any{
			"data_directory":            workspaceDir,
			"disable_default_providers": true,
		},
		"providers": map[string]any{
			"local": map[string]any{
				"base_url":             "http://127.0.0.1:1/v1",
				"auto_discover_models": false,
				"models":               []map[string]string{{"id": "local-model"}},
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "sennit.json"), globalConfig, 0o644))
	require.NoError(t, config.Trust(workingDir))

	workspaceConfig, err := json.Marshal(map[string]any{
		"env": map[string]string{
			"PROVIDERLOAD_CONFIG":         "config",
			"PROVIDERLOAD_CONFIG_DERIVED": "$PROVIDERLOAD_CONFIG",
			"PROVIDERLOAD_LATE_RESOLVED":  "$PROVIDERLOAD_LATE_PROCESS",
			"PROVIDERLOAD_OVERRIDE":       "config",
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workspaceDir, "sennit.json"), workspaceConfig, 0o644))

	store, err := config.LoadWithProcessor(workingDir, "", false, New())
	require.NoError(t, err)
	t.Setenv("PROVIDERLOAD_LATE_PROCESS", "after-load")

	resolved, err := store.Resolve("$PROVIDERLOAD_CONFIG_DERIVED/$PROVIDERLOAD_LATE_RESOLVED/$PROVIDERLOAD_OVERRIDE")
	require.NoError(t, err)
	require.Equal(t, "config/after-load/override", resolved)
}

func TestLoaderProcessesCustomProvider(t *testing.T) {
	cfg := customConfig(config.ProviderConfig{
		BaseURL:            "http://127.0.0.1:1/v1",
		AutoDiscoverModels: pointer(false),
		Models:             []catwalk.Model{{ID: "local-model"}},
	})
	result, err := New().Process(context.Background(), config.RuntimeInput{Config: cfg})
	require.NoError(t, err)
	require.Empty(t, result.KnownProviders)
	provider, ok := cfg.Providers.Get("local")
	require.True(t, ok)
	require.Equal(t, "local", provider.ID)
	require.Equal(t, config.ModelsSourceConfig, provider.ModelsSource)
}

func TestLoaderCustomProviderValidation(t *testing.T) {
	tests := []struct {
		name     string
		provider config.ProviderConfig
		problem  bool
	}{
		{name: "disabled", provider: config.ProviderConfig{Disable: true}},
		{name: "missing base URL", provider: config.ProviderConfig{Models: []catwalk.Model{{ID: "model"}}}, problem: true},
		{name: "unsupported type", provider: config.ProviderConfig{BaseURL: "http://localhost/v1", Type: "unsupported", Models: []catwalk.Model{{ID: "model"}}}, problem: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := customConfig(test.provider)
			_, err := New().Process(context.Background(), config.RuntimeInput{Config: cfg})
			require.Error(t, err)
			_, ok := cfg.Providers.Get("local")
			require.False(t, ok)
			require.Equal(t, test.problem, len(config.Doctor(cfg)) > 0)
		})
	}
}

func TestLoaderResolvesCustomProviderValues(t *testing.T) {
	t.Setenv("PROVIDER_HEADER", "header-value")
	t.Setenv("PROVIDER_PROXY", "http://proxy.test:8080")
	cfg := customConfig(config.ProviderConfig{
		BaseURL:            "http://localhost/v1",
		APIKey:             "",
		ExtraHeaders:       map[string]string{"X-Test": "$PROVIDER_HEADER"},
		ProxyURL:           "$PROVIDER_PROXY",
		AutoDiscoverModels: pointer(false),
		Models:             []catwalk.Model{{ID: "model"}},
	})
	result, err := New().Process(context.Background(), config.RuntimeInput{Config: cfg})
	require.NoError(t, err)
	provider, ok := result.RuntimeProviders.Get("local")
	require.True(t, ok)
	require.Equal(t, "header-value", provider.ExtraHeaders["X-Test"])
	require.Equal(t, "http://proxy.test:8080", provider.ProxyURL)
	// ProxyURL must track ProxyURL after load — it is the base
	// UpdateProviderAccount resolves an account's effective proxy from,
	// so a load that leaves it unset would make the very first account
	// switch forget the provider's own proxy.
	require.Equal(t, "http://proxy.test:8080", provider.ProxyURL)
	require.NotEmpty(t, config.Doctor(cfg))
}

// TestLoaderMergeCatalogProvidersSetsProxyURL covers the other
// load path that writes ProxyURL (mergeCatalogProviders, for providers that
// come from the embedded catalog rather than a user-defined "local" one):
// ProxyURL must be populated there too.
func TestLoaderMergeCatalogProvidersSetsProxyURL(t *testing.T) {
	cfg := &config.Config{
		Options: &config.Options{},
		Providers: csync.NewMap(map[string]config.ProviderConfig{
			"azure": {ProxyURL: "$AZURE_PROXY"},
		}),
	}
	environment := testEnvironment{
		"AZURE_ENDPOINT":           "https://azure.test",
		"AZURE_OPENAI_API_VERSION": "2026-01-01",
		"AZURE_PROXY":              "http://azure-proxy.test:8080",
	}
	_, err := New().mergeCatalogProviders(cfg, nil, environment, environment, []catwalk.Provider{{ID: catwalk.InferenceProviderAzure, APIEndpoint: "$AZURE_ENDPOINT", Models: []catwalk.Model{{ID: "model"}}}}, "", os.Stat)
	require.NoError(t, err)
	provider, ok := cfg.Providers.Get("azure")
	require.True(t, ok)
	require.Equal(t, "$AZURE_PROXY", provider.ProxyURL)
}

func TestLoaderCatalogCredentialPolicies(t *testing.T) {
	t.Run("Bedrock accepts credential file", func(t *testing.T) {
		home := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".aws"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(home, ".aws", "credentials"), []byte("[default]"), 0o600))
		cfg := catalogOnlyConfig()
		known, err := New().mergeCatalogProviders(cfg, nil, testEnvironment{}, config.IdentityResolver(), []catwalk.Provider{{ID: catwalk.InferenceProviderBedrock, Models: []catwalk.Model{{ID: "model"}}}}, home, os.Stat)
		require.NoError(t, err)
		require.True(t, known["bedrock"])
		_, ok := cfg.Providers.Get("bedrock")
		require.True(t, ok)
	})

	t.Run("Azure resolves endpoint and API version", func(t *testing.T) {
		cfg := catalogOnlyConfig()
		environment := testEnvironment{"AZURE_ENDPOINT": "https://azure.test", "AZURE_OPENAI_API_VERSION": "2026-01-01"}
		_, err := New().mergeCatalogProviders(cfg, nil, environment, environment, []catwalk.Provider{{ID: catwalk.InferenceProviderAzure, APIEndpoint: "$AZURE_ENDPOINT", Models: []catwalk.Model{{ID: "model"}}}}, "", os.Stat)
		require.NoError(t, err)
		provider, ok := cfg.Providers.Get("azure")
		require.True(t, ok)
		require.Equal(t, "https://azure.test", provider.BaseURL)
	})

	t.Run("Vertex records project and location", func(t *testing.T) {
		cfg := catalogOnlyConfig()
		environment := testEnvironment{"VERTEXAI_PROJECT": "project", "VERTEXAI_LOCATION": "location"}
		_, err := New().mergeCatalogProviders(cfg, nil, environment, environment, []catwalk.Provider{{ID: catwalk.InferenceProviderVertexAI, Models: []catwalk.Model{{ID: "model"}}}}, "", os.Stat)
		require.NoError(t, err)
		_, ok := cfg.Providers.Get("vertexai")
		require.True(t, ok)
	})

	t.Run("missing key drops explicitly configured catalog provider", func(t *testing.T) {
		cfg := catalogOnlyConfig()
		cfg.Providers.Set("remote", config.ProviderConfig{})
		environment := testEnvironment{}
		_, err := New().mergeCatalogProviders(cfg, nil, environment, environment, []catwalk.Provider{{ID: "remote", APIKey: "$MISSING", Models: []catwalk.Model{{ID: "model"}}}}, "", os.Stat)
		require.NoError(t, err)
		_, ok := cfg.Providers.Get("remote")
		require.False(t, ok)
		require.NotEmpty(t, config.Doctor(cfg))
	})
}

func TestLoaderRemovesStaleAnthropicOAuth(t *testing.T) {
	cfg := catalogOnlyConfig()
	cfg.Providers.Set("anthropic", config.ProviderConfig{OAuthToken: &oauth.Token{AccessToken: "token"}})
	store := &recordingStore{}
	_, err := New().mergeCatalogProviders(cfg, store, testEnvironment{}, config.IdentityResolver(), []catwalk.Provider{{ID: catwalk.InferenceProviderAnthropic, APIKey: "key"}}, "", os.Stat)
	require.NoError(t, err)
	require.Equal(t, []string{"providers.anthropic"}, store.removed)
	_, ok := cfg.Providers.Get("anthropic")
	require.False(t, ok)
}

func customConfig(provider config.ProviderConfig) *config.Config {
	return &config.Config{
		Options: &config.Options{DisableDefaultProviders: true},
		Providers: csync.NewMap(map[string]config.ProviderConfig{
			"local": provider,
		}),
	}
}

func catalogOnlyConfig() *config.Config {
	return &config.Config{Options: &config.Options{}, Providers: csync.NewMap[string, config.ProviderConfig]()}
}

type testEnvironment map[string]string

func (environment testEnvironment) Get(key string) string { return environment[key] }
func (environment testEnvironment) Env() []string {
	values := make([]string, 0, len(environment))
	for key, value := range environment {
		values = append(values, key+"="+value)
	}
	return values
}

func (environment testEnvironment) ResolveValue(value string) (string, error) {
	if len(value) > 1 && value[0] == '$' {
		return environment[value[1:]], nil
	}
	return value, nil
}

func pointer(value bool) *bool { return &value }
