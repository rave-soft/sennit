package config

import (
	"path/filepath"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

// TestModelCache_SaveLoadRoundTrip verifies that models saved for a
// provider come back unchanged, and that fetching an unknown provider is a
// clean miss rather than an error.
func TestModelCache_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	globalDataPath := filepath.Join(dir, "sennit.json")

	models := []catwalk.Model{
		{ID: "model-a", Name: "Model A"},
		{ID: "model-b", Name: "Model B"},
	}
	saveCachedModels(globalDataPath, "custom", models)

	got, ok := loadCachedModels(globalDataPath, "custom")
	require.True(t, ok)
	require.Equal(t, models, got)
}

// TestModelCache_LoadMissingProvider verifies that a provider never saved
// to the cache reports ok=false without an error or panic, both when the
// db file doesn't exist at all and when it exists but lacks the row.
func TestModelCache_LoadMissingProvider(t *testing.T) {
	dir := t.TempDir()
	globalDataPath := filepath.Join(dir, "sennit.json")

	// No models.db has been created yet.
	got, ok := loadCachedModels(globalDataPath, "nope")
	require.False(t, ok)
	require.Nil(t, got)

	// models.db exists (another provider was saved) but "nope" was not.
	saveCachedModels(globalDataPath, "other", []catwalk.Model{{ID: "m"}})
	got, ok = loadCachedModels(globalDataPath, "nope")
	require.False(t, ok)
	require.Nil(t, got)
}

// TestModelCache_ConcurrentAccess exercises concurrent save/load calls
// against the same cache to catch corruption or deadlocks; run with
// -race.
func TestModelCache_ConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	globalDataPath := filepath.Join(dir, "sennit.json")

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			providerID := "provider"
			models := []catwalk.Model{{ID: "model", Name: "Model"}}
			saveCachedModels(globalDataPath, providerID, models)
			_, _ = loadCachedModels(globalDataPath, providerID)
			_ = i
		})
	}
	wg.Wait()

	got, ok := loadCachedModels(globalDataPath, "provider")
	require.True(t, ok)
	require.Len(t, got, 1)
	require.Equal(t, "model", got[0].ID)
}

func TestModelCache_RefreshKeepsContextWindow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	globalDataPath := filepath.Join(dir, "sennit.json")

	saveCachedModels(globalDataPath, "custom", []catwalk.Model{
		{ID: "gpt-x", ContextWindow: 1050000, DefaultMaxTokens: 128000, CostPer1MIn: 2, CostPer1MOut: 8},
		{ID: "gone", ContextWindow: 4096},
	})

	// A refresh from a plain /models endpoint carries no metadata: cached
	// context window, token limits, and pricing must survive; a non-zero
	// fresh value wins over the cached one.
	saveCachedModels(globalDataPath, "custom", []catwalk.Model{
		{ID: "gpt-x", CostPer1MIn: 3},
		{ID: "gpt-y", ContextWindow: 8192},
	})

	got, ok := loadCachedModels(globalDataPath, "custom")
	require.True(t, ok)
	require.Len(t, got, 2)
	require.Equal(t, "gpt-x", got[0].ID)
	require.Equal(t, int64(1050000), got[0].ContextWindow)
	require.Equal(t, int64(128000), got[0].DefaultMaxTokens)
	require.Equal(t, float64(3), got[0].CostPer1MIn)
	require.Equal(t, float64(8), got[0].CostPer1MOut)
	require.Equal(t, int64(8192), got[1].ContextWindow)
}
