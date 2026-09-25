package appws

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/csync"
	providerstate "github.com/rave-soft/sennit/internal/providers/state"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// newConfigCacheTestWorkspace builds a real AppWorkspace over a real,
// disk-backed ConfigStore (the same recipe newOAuthTestWorkspace uses),
// so SetConfigField's write-then-reload path actually swaps the store's
// published *config.Config pointer - the signal Config()'s cache keys on.
func newConfigCacheTestWorkspace(t *testing.T) *AppWorkspace {
	t.Helper()
	globalConfigDir := t.TempDir()
	globalDataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalConfigDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDataDir)
	require.NoError(t, os.WriteFile(filepath.Join(globalDataDir, "sennit.json"), []byte("{}"), 0o600))

	store, err := configruntime.Load(t.TempDir(), t.TempDir(), false)
	require.NoError(t, err)

	a := app.NewForTest(t.Context())
	a.SetConfigForTest(store)
	t.Cleanup(a.ShutdownForTest)

	return NewAppWorkspace(a, store)
}

// TestAppWorkspace_Config_CachesUntilReload is the cache half of the PR
// 0.5 performance finding: the UI calls Config() several times per
// rendered frame, and rebuilding the DTO (which embeds the model catalog)
// on every call was O(providers x models) allocation per call. Two calls
// with no reload in between must return the identical *FrontendConfig
// pointer.
func TestAppWorkspace_Config_CachesUntilReload(t *testing.T) {
	ws := newConfigCacheTestWorkspace(t)

	first := ws.Config()
	second := ws.Config()
	require.Same(t, first, second, "Config() must return the cached DTO when the store's config snapshot hasn't changed")
}

// TestAppWorkspace_Config_ReflectsChangeAfterReload is the invalidation
// half: a config write (through the store, the way SetConfigField/a
// reload does in production) must publish a new config.Config pointer,
// which must in turn produce a new *FrontendConfig reflecting the change -
// the cache must never paper over an update with a stale snapshot.
func TestAppWorkspace_Config_ReflectsChangeAfterReload(t *testing.T) {
	ws := newConfigCacheTestWorkspace(t)

	before := ws.Config()
	require.Equal(t, "AGENTS.md", before.InitializeAs, "the default, before any write")

	require.NoError(t, ws.store.SetConfigField(config.ScopeGlobal, "options.initialize_as", "SENNIT.md"))

	after := ws.Config()
	require.NotSame(t, before, after, "a config change must invalidate the cached DTO")
	require.Equal(t, "SENNIT.md", after.InitializeAs)

	// And the cache re-engages once more: two reads after the change,
	// with no further write, return the same pointer again.
	require.Same(t, after, ws.Config())
}

// TestAppWorkspace_Config_ConcurrentReadsAndReload races many goroutines
// calling Config() (the Update goroutine and tea.Cmd goroutines both do,
// concurrently, in production) against a config write landing partway
// through - the shape TestAppWorkspace_Config_CachesUntilReload and
// TestAppWorkspace_Config_ReflectsChangeAfterReload check sequentially.
// Run with -race: the cache is an atomic.Pointer specifically so this
// never tears.
func TestAppWorkspace_Config_ConcurrentReadsAndReload(t *testing.T) {
	ws := newConfigCacheTestWorkspace(t)

	const readers = 16
	start := make(chan struct{})
	done := make(chan struct{})
	for range readers {
		go func() {
			<-start
			for range 200 {
				if dto := ws.Config(); dto == nil {
					t.Error("Config() returned nil")
				}
			}
			done <- struct{}{}
		}()
	}

	close(start)
	require.NoError(t, ws.store.SetConfigField(config.ScopeGlobal, "options.initialize_as", "SENNIT.md"))
	for range readers {
		<-done
	}

	require.Equal(t, "SENNIT.md", ws.Config().InitializeAs)
}

// BenchmarkAppWorkspaceConfig_Cached measures Config() as the UI actually
// calls it: repeatedly, against an unchanging store, with the embedded
// catalog populated (see fixture below) so a regression back to rebuilding
// the DTO on every call shows up here.
func BenchmarkAppWorkspaceConfig_Cached(b *testing.B) {
	ws := newConfigCacheBenchWorkspace(b)

	b.ReportAllocs()
	for b.Loop() {
		_ = ws.Config()
	}
}

// BenchmarkAppWorkspaceConfig_Uncached measures NewFrontendConfig directly
// (what Config() did before this cache existed) against the same fixture,
// as the baseline the cached benchmark above is meant to beat.
func BenchmarkAppWorkspaceConfig_Uncached(b *testing.B) {
	ws := newConfigCacheBenchWorkspace(b)
	src := ws.store.Config()
	known := ws.store.KnownProviders()

	b.ReportAllocs()
	for b.Loop() {
		_ = workspace.NewFrontendConfig(src, known)
	}
}

// benchProviderCount and benchModelsPerProvider size the fixture these
// benchmarks build: 20 configured providers x 50 models each is well
// within what a real sennitrc with a couple of custom OpenAI-compatible
// endpoints and the full embedded catalog looks like, and is large enough
// that a regression back to rebuilding the DTO (and cloning every runtime
// provider's Models/ExtraHeaders/ExtraParams/OAuth token) on every
// Config() call shows up clearly in b.ReportAllocs() output.
const (
	benchProviderCount     = 20
	benchModelsPerProvider = 50
)

// newConfigCacheBenchFixture builds a *config.Config with
// benchProviderCount providers, each carrying benchModelsPerProvider
// catalog models and a live runtime entry (so newFrontendProvider's Auth
// projection has real work to do). configtest.NewStore (unlike the disk
// load pipeline) never populates ConfigStore.KnownProviders, so every
// provider here is "custom" from NewFrontendConfig's standpoint - that
// still exercises isCustomProvider's catalog walk on every call, just
// against an empty catalog; both benchmarks below read
// ws.store.KnownProviders() for the comparison to stay apples-to-apples.
func newConfigCacheBenchFixture() *config.Config {
	providers := csync.NewMap[string, config.ProviderConfig]()
	cfg := &config.Config{Options: &config.Options{}}
	for i := range benchProviderCount {
		id := fmt.Sprintf("provider-%02d", i)
		models := make([]catwalk.Model, benchModelsPerProvider)
		for m := range models {
			models[m] = catwalk.Model{ID: fmt.Sprintf("model-%02d", m), Name: fmt.Sprintf("Model %02d", m), CanReason: m%2 == 0}
		}
		providers.Set(id, config.ProviderConfig{
			ID: id, Name: id, BaseURL: "https://api.example.com/v1", APIKey: "sk-test", Models: models,
		})
		cfg.SetRuntimeProvider(id, providerstate.Provider{ID: id, APIKey: "sk-test", Models: models})
	}
	cfg.Providers = providers
	return cfg
}

func newConfigCacheBenchWorkspace(tb testing.TB) *AppWorkspace {
	tb.Helper()
	store := configtest.NewStore(tb, newConfigCacheBenchFixture(), configtest.WithLoadedPaths(tb.TempDir()))

	a := app.NewForTest(tb.Context())
	a.SetConfigForTest(store)

	return NewAppWorkspace(a, store)
}
