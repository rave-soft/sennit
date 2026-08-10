package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/embedded"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestConfig_Load_MigratesBloatedModelCache verifies the one-time migration
// that moves a custom provider's bloated models array, left over from
// before the model-discovery cache existed, out of the data-dir config
// file and into the cache — leaving a known catalog provider's models
// override untouched.
func TestConfig_Load_MigratesBloatedModelCache(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("BRAID_GLOBAL_CONFIG", globalDir)
	t.Setenv("BRAID_GLOBAL_DATA", dataDir)

	dataConfigPath := GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(dataConfigPath), 0o755))
	seed := `{
		"providers": {
			"omniroute": {
				"api_key": "test-key",
				"base_url": "http://127.0.0.1:1/v1",
				"models": [
					{"id": "model-1", "name": "Model 1"},
					{"id": "model-2", "name": "Model 2"},
					{"id": "model-3", "name": "Model 3"}
				]
			}
		}
	}`
	require.NoError(t, os.WriteFile(dataConfigPath, []byte(seed), 0o644))

	workingDir := t.TempDir()
	_, err := Load(workingDir, "", false)
	require.NoError(t, err)

	// The on-disk data-dir file no longer carries the bloated models array.
	after, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(after, "providers.omniroute.models").Exists())

	// The models moved into the cache.
	cached, ok := loadCachedModels(dataConfigPath, "omniroute")
	require.True(t, ok)
	require.Len(t, cached, 3)
	require.Equal(t, "model-1", cached[0].ID)

	// A second, independent Load sees the migrated models via the cache
	// without needing to touch the (unreachable) network.
	workingDir2 := t.TempDir()
	store2, err := Load(workingDir2, "", false)
	require.NoError(t, err)
	pc, ok := store2.config.Providers.Get("omniroute")
	require.True(t, ok)
	require.Len(t, pc.Models, 3)
}

// TestConfig_Load_MigrationLeavesKnownProviderModelsUntouched verifies
// that a known catalog provider's models override (a legitimate user
// customization, not a discovery dump) is never touched by the migration.
func TestConfig_Load_MigrationLeavesKnownProviderModelsUntouched(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("BRAID_GLOBAL_CONFIG", globalDir)
	t.Setenv("BRAID_GLOBAL_DATA", dataDir)

	dataConfigPath := GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(dataConfigPath), 0o755))

	knownProviders := embedded.GetAll()
	require.NotEmpty(t, knownProviders, "embedded catalog must not be empty for this test to be meaningful")
	knownID := string(knownProviders[0].ID)

	seed := fmt.Sprintf(`{
		"providers": {
			%q: {
				"api_key": "test-key",
				"models": [{"id": "override-model", "name": "Override Model"}]
			}
		}
	}`, knownID)
	require.NoError(t, os.WriteFile(dataConfigPath, []byte(seed), 0o644))

	before, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)

	workingDir := t.TempDir()
	_, err = Load(workingDir, "", false)
	require.NoError(t, err)

	after, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "known catalog provider's models override must not be migrated")

	_, ok := loadCachedModels(dataConfigPath, knownID)
	require.False(t, ok, "known catalog provider's models must not be cached as a discovery result")
}
