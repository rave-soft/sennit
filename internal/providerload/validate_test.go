package providerload

import (
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/modelcache"
	"github.com/stretchr/testify/require"
)

// TestValidateCustomProvidersCacheFedDiscoverySavesFullResult guards against
// a bug where a provider whose Models list was pre-filled from the
// model-discovery cache (ModelsSource == Cache, set by
// resolveCustomProviderModels before this method runs) was mistaken for one
// with hand-written models. That misclassification made validateCustomProviders
// persist only the delta of models newly returned by this discovery round,
// overwriting the cache row and permanently dropping every previously
// cached model the endpoint didn't happen to return again this time.
func TestValidateCustomProvidersCacheFedDiscoverySavesFullResult(t *testing.T) {
	globalDataPath := filepath.Join(t.TempDir(), "sennit.json")

	// Simulate resolveCustomProviderModels having pre-filled Models {A, B}
	// from the cache before discovery ran.
	provider := config.ProviderConfig{
		BaseURL:      "http://localhost/v1",
		Models:       []catwalk.Model{{ID: "A"}, {ID: "B"}},
		ModelsSource: config.ModelsSourceCache,
	}
	cfg := &config.Config{
		Options:   &config.Options{DisableDefaultProviders: true},
		Providers: csync.NewMap(map[string]config.ProviderConfig{"local": provider}),
	}

	// This discovery round only returned {A, B, C} — as if C is newly added
	// upstream. The pre-existing cache row for "local" already holds {A, B}.
	modelcache.New(globalDataPath).SaveBestEffort("local", []catwalk.Model{{ID: "A"}, {ID: "B"}})

	discoveryResults := map[string]discoveryResult{
		"local": {models: []catwalk.Model{{ID: "A"}, {ID: "B"}, {ID: "C"}}},
	}

	err := New().validateCustomProviders(cfg, map[string]bool{}, config.IdentityResolver(), discoveryResults, globalDataPath)
	require.NoError(t, err)

	got, ok := cfg.Providers.Get("local")
	require.True(t, ok)
	require.Equal(t, config.ModelsSourceCache, got.ModelsSource)
	require.ElementsMatch(t, []string{"A", "B", "C"}, modelIDs(got.Models))

	// The cache must retain the full merged set, not just the newly
	// discovered delta ({C}) — otherwise a later load with the endpoint
	// down would come back with only C.
	cached, ok := modelcache.New(globalDataPath).Load("local")
	require.True(t, ok)
	require.ElementsMatch(t, []string{"A", "B", "C"}, modelIDs(cached))
}

func modelIDs(models []catwalk.Model) []string {
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	return ids
}
