package modelsrefresh

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/discover"
	"github.com/rave-soft/sennit/internal/modelcache"
)

// fakeDiscoverer returns the configured models for the given provider ID,
// recording every call. No network access happens: it stands in for
// discover.DiscoverModels through the RefreshWith seam.
type fakeDiscoverer struct {
	modelsFor map[string][]catwalk.Model
	err       error
	calls     []string
}

func (f *fakeDiscoverer) discover(_ context.Context, cfg discover.Config, _ discover.Resolver) ([]catwalk.Model, error) {
	f.calls = append(f.calls, cfg.ID)
	if f.err != nil {
		return nil, f.err
	}
	return append([]catwalk.Model(nil), f.modelsFor[cfg.ID]...), nil
}

func boolPtr(value bool) *bool { return &value }

// refreshStore is a hand-rolled config store over a per-test working
// directory; withGlobalDataPath points its cache writes at a temp dir.
type refreshStore struct {
	t *testing.T
	*config.ConfigStore
}

func newRefreshStore(t *testing.T, workingDir string, providers ...config.ProviderConfig) *refreshStore {
	t.Helper()
	providersMap := csync.NewMap[string, config.ProviderConfig]()
	for _, provider := range providers {
		providersMap.Set(provider.ID, provider)
	}
	cfg := &config.Config{Providers: providersMap}
	return &refreshStore{t: t, ConfigStore: configtest.NewStore(t, cfg, configtest.WithWorkingDir(workingDir))}
}

// withGlobalDataPath rebuilds the underlying store at the same config and
// working directory, with the global data path pointed at the seeded
// global config so cache writes land in the test's temp dir.
func (s *refreshStore) withGlobalDataPath(path string) {
	s.ConfigStore = configtest.NewStore(s.t, s.Config(),
		configtest.WithWorkingDir(s.WorkingDir()),
		configtest.WithGlobalDataPath(path))
}

// seedGlobalConfig writes a global config file at globalDir.
func seedGlobalConfig(t *testing.T, globalDir, seed string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "sennit.json"), []byte(seed), 0o644))
}

// disabledProviderSeed is a global config with one custom provider that
// carries a hand-written model list and the default providers disabled,
// so the load pipeline makes no discovery requests.
const disabledProviderSeed = `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`

// catalogSeed re-enables the embedded catalog on top of the same mock
// provider, so the load pipeline populates the store's known providers
// while still making no discovery requests.
const catalogSeed = `{
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`

// TestRefreshWithSelection pins who RefreshWith may touch: an explicit
// custom provider runs and its cache is written, and an unknown ID is reported as not found rather than
// refreshing nothing silently.
func TestRefreshWithSelection(t *testing.T) {
	t.Parallel()

	globalDir := t.TempDir()
	seedGlobalConfig(t, globalDir, disabledProviderSeed)
	store := newRefreshStore(t, t.TempDir(),
		config.ProviderConfig{ID: "openai", BaseURL: "https://api.openai.com/v1", APIKey: "key"},
		config.ProviderConfig{ID: "custom", BaseURL: "http://127.0.0.1:11434/v1"},
	)
	store.withGlobalDataPath(filepath.Join(globalDir, "sennit.json"))

	fake := &fakeDiscoverer{modelsFor: map[string][]catwalk.Model{
		"custom": {{ID: "m1", Name: "M1"}},
	}}

	t.Run("explicit custom provider is refreshed", func(t *testing.T) {
		t.Parallel()
		results, err := RefreshWith(t.Context(), store.ConfigStore, "custom", fake.discover)
		require.NoError(t, err)
		require.Len(t, results, 1)
		require.Equal(t, "custom", results[0].ID)
		require.False(t, results[0].Skipped)
		require.NoError(t, results[0].Err)
		require.Equal(t, []string{"custom"}, fake.calls)
		require.FileExists(t, filepath.Join(globalDir, "models.db"))
	})

	t.Run("unknown provider is rejected", func(t *testing.T) {
		t.Parallel()
		_, err := RefreshWith(t.Context(), store.ConfigStore, "missing", fake.discover)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not found")
	})
}

// TestRefreshWithCatalogProviderRejected drives the real load pipeline so
// the store carries the embedded catalog, and pins that RefreshWith
// refuses a catalog provider ID rather than discovering from it. It is
// not parallel because t.Setenv forbids it.
func TestRefreshWithCatalogProviderRejected(t *testing.T) {
	globalDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDir)
	seedGlobalConfig(t, globalDir, catalogSeed)

	store, err := configruntime.Load(t.TempDir(), "", false)
	require.NoError(t, err)
	require.NotEmpty(t, store.KnownProviders(), "the load pipeline must expose the embedded catalog")

	fake := &fakeDiscoverer{modelsFor: map[string][]catwalk.Model{"openai": {{ID: "m1"}}}}
	_, err = RefreshWith(t.Context(), store, "openai", fake.discover)
	require.Error(t, err)
	require.Contains(t, err.Error(), "catalog provider")
	require.Empty(t, fake.calls, "a catalog provider must never reach the discoverer")
}

// TestRefreshWithEmptyProviderIDRefreshesOnlyEligibleProviders pins that an
// empty providerID fans out over custom providers only: catalog providers,
// disabled ones, and base_url-less ones are excluded.
func TestRefreshWithEmptyProviderIDRefreshesOnlyEligibleProviders(t *testing.T) {
	t.Parallel()

	globalDir := t.TempDir()
	seedGlobalConfig(t, globalDir, disabledProviderSeed)
	store := newRefreshStore(t, t.TempDir(),
		config.ProviderConfig{ID: "custom", BaseURL: "http://127.0.0.1:11434/v1"},
		config.ProviderConfig{ID: "custom2", BaseURL: "http://127.0.0.1:11435/v1", Disable: true},
		config.ProviderConfig{ID: "custom3"},
	)
	store.withGlobalDataPath(filepath.Join(globalDir, "sennit.json"))

	fake := &fakeDiscoverer{modelsFor: map[string][]catwalk.Model{
		"custom": {{ID: "m1", Name: "M1"}},
	}}

	results, err := RefreshWith(t.Context(), store.ConfigStore, "", fake.discover)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "custom", results[0].ID)
	require.False(t, results[0].Skipped)
	require.NoError(t, results[0].Err, "the cache write must hit the seeded global path")
	require.Equal(t, []string{"custom"}, fake.calls)
}

// TestRefreshWithGuards pins the two guards: discover_models: false is a
// failure that never reaches the discoverer, and a
// hand-written model list (ModelsSource == config.ModelsSourceConfig) is
// skipped unless discover_models is explicitly true.
func TestRefreshWithGuards(t *testing.T) {
	t.Parallel()

	t.Run("discover_models false is a failure", func(t *testing.T) {
		t.Parallel()
		fake := &fakeDiscoverer{}
		store := newRefreshStore(t, t.TempDir(), config.ProviderConfig{
			ID: "custom", BaseURL: "http://127.0.0.1:11434/v1", AutoDiscoverModels: boolPtr(false),
		})

		results, err := RefreshWith(t.Context(), store.ConfigStore, "custom", fake.discover)
		require.NoError(t, err)
		require.Len(t, results, 1)
		require.False(t, results[0].Skipped)
		require.ErrorIs(t, results[0].Err, ErrDiscoveryDisabled)
		require.Empty(t, fake.calls, "the discoverer must not run for a hard stop")
	})

	t.Run("hand-written models are skipped without discover_models", func(t *testing.T) {
		t.Parallel()
		fake := &fakeDiscoverer{}
		store := newRefreshStore(t, t.TempDir(), config.ProviderConfig{
			ID: "custom", BaseURL: "http://127.0.0.1:11434/v1",
			Models:       []catwalk.Model{{ID: "hand", Name: "Hand"}},
			ModelsSource: config.ModelsSourceConfig,
		})

		results, err := RefreshWith(t.Context(), store.ConfigStore, "custom", fake.discover)
		require.NoError(t, err)
		require.Len(t, results, 1)
		require.True(t, results[0].Skipped)
		require.Equal(t, "models are explicitly defined in config", results[0].SkipReason)
		require.Empty(t, fake.calls)
	})

	t.Run("hand-written models are refreshed with discover_models true", func(t *testing.T) {
		t.Parallel()
		fake := &fakeDiscoverer{modelsFor: map[string][]catwalk.Model{
			"custom": {{ID: "fresh", Name: "Fresh"}},
		}}
		store := newRefreshStore(t, t.TempDir(), config.ProviderConfig{
			ID: "custom", BaseURL: "http://127.0.0.1:11434/v1", AutoDiscoverModels: boolPtr(true),
			Models:       []catwalk.Model{{ID: "hand", Name: "Hand"}},
			ModelsSource: config.ModelsSourceConfig,
		})

		results, err := RefreshWith(t.Context(), store.ConfigStore, "custom", fake.discover)
		require.NoError(t, err)
		require.Len(t, results, 1)
		require.False(t, results[0].Skipped)
		require.NoError(t, results[0].Err)
		require.Equal(t, 1, results[0].Added)
		require.Equal(t, 1, results[0].Removed)
	})
}

// TestRefreshWithHappyPath pins the full write path: diff counts against
// the existing model list and the cache database written next to the
// global config path. It is not parallel because t.Setenv forbids it.
func TestRefreshWithHappyPath(t *testing.T) {
	globalDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDir)
	seedGlobalConfig(t, globalDir, disabledProviderSeed)

	// The hand-rolled store shares the seeded global config through its
	// global data path, so the cache write lands in globalDir.
	store := newRefreshStore(t, t.TempDir(), config.ProviderConfig{
		ID:      "mock",
		Name:    "Mock",
		Type:    catwalk.TypeOpenAI,
		BaseURL: "http://127.0.0.1:9/v1",
		APIKey:  "test-key",
		Models:  []catwalk.Model{{ID: "shared", Name: "Shared"}, {ID: "gone", Name: "Gone"}},
		// The in-memory list stands in for a cache-sourced one; the
		// skip guard only fires for config-sourced lists without
		// discover_models.
		ModelsSource: config.ModelsSourceCache,
	})
	store.withGlobalDataPath(filepath.Join(globalDir, "sennit.json"))

	fake := &fakeDiscoverer{modelsFor: map[string][]catwalk.Model{
		"mock": {{ID: "shared", Name: "Shared"}, {ID: "new", Name: "New"}},
	}}
	configPath, err := store.ConfigPath(config.ScopeGlobal)
	require.NoError(t, err)

	// Seed the cache the way a previous discovery run would have, so
	// Save's MergeMetadata has cached metadata to keep: the stored
	// 'shared' model then carries a context window the fake discoverer
	// does not supply, and 'new' is added with none.
	require.NoError(t, modelcache.New(configPath).Save("mock", []catwalk.Model{
		{ID: "shared", Name: "Shared", ContextWindow: 8192},
	}))

	results, err := RefreshWith(t.Context(), store.ConfigStore, "mock", fake.discover)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.False(t, results[0].Skipped)
	require.NoError(t, results[0].Err)
	require.Equal(t, 1, results[0].Added, "only 'new' is absent from the existing list")
	require.Equal(t, 1, results[0].Removed, "only 'gone' is absent from the fresh list")
	require.Equal(t, 2, results[0].Models)

	require.FileExists(t, filepath.Join(globalDir, "models.db"))

	cached, ok := modelcache.New(configPath).Load("mock")
	require.True(t, ok)
	require.Equal(t, []string{"shared", "new"}, modelIDs(cached))
	require.Equal(t, int64(8192), cached[0].ContextWindow, "Save must keep cached metadata for a model the discovery response does not describe")
}

// TestRefreshWithDiscoveryFailure pins that a discoverer error surfaces on
// the provider's result rather than failing the whole fan-out, and that a
// nil store is reported as a configuration error.
func TestRefreshWithDiscoveryFailure(t *testing.T) {
	t.Parallel()

	fake := &fakeDiscoverer{err: fmt.Errorf("endpoint down")}
	store := newRefreshStore(t, t.TempDir(), config.ProviderConfig{
		ID: "custom", BaseURL: "http://127.0.0.1:11434/v1",
	})

	results, err := RefreshWith(t.Context(), store.ConfigStore, "custom", fake.discover)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	require.Contains(t, results[0].Err.Error(), "endpoint down")

	_, err = RefreshWith(t.Context(), nil, "", fake.discover)
	require.Error(t, err)
}

func modelIDs(models []catwalk.Model) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

// TestDiffModelIDsCountsDuplicateIDsOnce pins that a discovery response
// listing one ID twice counts it once.
func TestDiffModelIDsCountsDuplicateIDsOnce(t *testing.T) {
	t.Parallel()

	added, removed := DiffModelIDs(
		[]catwalk.Model{{ID: "old"}},
		[]catwalk.Model{{ID: "new"}, {ID: "new"}},
	)
	require.Equal(t, 1, added)
	require.Equal(t, 1, removed)
}

// TestRefreshCodexSignedOut pins that Refresh routes "codex" to the Codex
// path rather than rejecting it as a catalog provider, and that without a
// login the request fails before any network call.
func TestRefreshCodexSignedOut(t *testing.T) {
	globalDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDir)
	seedGlobalConfig(t, globalDir, `{"providers": {"codex": {"proxy_url": "http://127.0.0.1:9"}}}`)

	store, err := configruntime.Load(t.TempDir(), "", false)
	require.NoError(t, err)
	require.False(t, CodexConfigured(store))

	results, err := Refresh(t.Context(), store, "codex")
	require.ErrorIs(t, err, ErrCodexSignedOut)
	require.Nil(t, results)
}
