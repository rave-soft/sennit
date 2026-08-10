package config

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"
	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/env"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestConfig_Load_MigratesBloatedModelCache verifies the one-time migration
// that moves a custom provider's bloated models array, left over from
// before the model-discovery cache existed, out of the data-dir config
// file and into the cache — leaving a known catalog provider's models
// override untouched. The array must exceed modelCacheMigrationThreshold
// (a real discovery dump, like the 1239-model omniroute catalog that
// prompted this cache) — see
// TestConfig_Load_SmallManualModelListIsNotMigrated for the other side of
// that threshold.
func TestConfig_Load_MigratesBloatedModelCache(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("BRAID_GLOBAL_CONFIG", globalDir)
	t.Setenv("BRAID_GLOBAL_DATA", dataDir)

	dataConfigPath := GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(dataConfigPath), 0o755))

	var modelsJSON bytes.Buffer
	modelsJSON.WriteString("[")
	const seedCount = modelCacheMigrationThreshold + 10
	for i := range seedCount {
		if i > 0 {
			modelsJSON.WriteString(",")
		}
		fmt.Fprintf(&modelsJSON, `{"id": "model-%d", "name": "Model %d"}`, i, i)
	}
	modelsJSON.WriteString("]")

	seed := fmt.Sprintf(`{
		"providers": {
			"omniroute": {
				"api_key": "test-key",
				"base_url": "http://127.0.0.1:1/v1",
				"models": %s
			}
		}
	}`, modelsJSON.String())
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
	require.Len(t, cached, seedCount)
	require.Equal(t, "model-0", cached[0].ID)

	// A second, independent Load sees the migrated models via the cache
	// without needing to touch the (unreachable) network.
	workingDir2 := t.TempDir()
	store2, err := Load(workingDir2, "", false)
	require.NoError(t, err)
	pc, ok := store2.config.Providers.Get("omniroute")
	require.True(t, ok)
	require.Len(t, pc.Models, seedCount)
}

// TestConfig_Load_SmallManualModelListIsNotMigrated is the regression test
// for the incident that prompted modelCacheMigrationThreshold: a
// llama.cpp provider ("qwen36-local") with 3 hand-written models got swept
// up by an unconditional migration as if it were a bloated discovery dump,
// moved into the cache, and then lost when a later refresh replaced the
// cache with a single junk gguf-path "model". A short models array must be
// left exactly where the user put it — in the config file, not the cache —
// so it is never subject to being silently overwritten by discovery.
func TestConfig_Load_SmallManualModelListIsNotMigrated(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("BRAID_GLOBAL_CONFIG", globalDir)
	t.Setenv("BRAID_GLOBAL_DATA", dataDir)

	dataConfigPath := GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(dataConfigPath), 0o755))
	seed := `{
		"providers": {
			"qwen36-local": {
				"api_key": "test-key",
				"base_url": "http://127.0.0.1:1/v1",
				"discover_models": false,
				"models": [
					{"id": "qwen3.6-30b-instruct", "name": "Qwen3.6 30B Instruct"},
					{"id": "qwen3.6-30b-thinking", "name": "Qwen3.6 30B Thinking"},
					{"id": "qwen3.6-14b-instruct", "name": "Qwen3.6 14B Instruct"}
				]
			}
		}
	}`
	require.NoError(t, os.WriteFile(dataConfigPath, []byte(seed), 0o644))

	workingDir := t.TempDir()
	store, err := Load(workingDir, "", false)
	require.NoError(t, err)

	// Still on disk, untouched — migration must be a no-op below the
	// threshold.
	after, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(after, "providers.qwen36-local.models").Exists())
	require.Len(t, gjson.GetBytes(after, "providers.qwen36-local.models").Array(), 3)

	// Never copied into the cache either — there is nothing to migrate.
	_, ok := loadCachedModels(dataConfigPath, "qwen36-local")
	require.False(t, ok)

	// The manual list is what the provider actually loaded with, marked as
	// config-sourced so `braid models refresh` refuses to replace it.
	pc, ok := store.config.Providers.Get("qwen36-local")
	require.True(t, ok)
	require.Len(t, pc.Models, 3)
	require.Equal(t, ModelsSourceConfig, pc.ModelsSource)
}

// TestConfig_Load_DiscoverModelsFalseNeverDiscovers verifies the hard-stop
// guard in discoverCustomProviderModels: an explicit discover_models: false
// must never trigger HTTP discovery, whether or not Models is empty. This
// is what should have prevented the qwen36-local incident's junk gguf-path
// "model" from ever reaching the config once the user set discover_models:
// false to work around llama.cpp's non-standard /v1/models response.
func TestConfig_Load_DiscoverModelsFalseNeverDiscovers(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("BRAID_GLOBAL_CONFIG", globalDir)
	t.Setenv("BRAID_GLOBAL_DATA", dataDir)

	discoverFalse := false
	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"qwen36-local": {
				APIKey:             "test-key",
				BaseURL:            "http://127.0.0.1:1/v1", // unreachable; discovery must never even be attempted
				AutoDiscoverModels: &discoverFalse,
				Models: []catwalk.Model{
					{ID: "qwen3.6-30b-instruct", Name: "Qwen3.6 30B Instruct"},
				},
			},
		}),
	}
	cfg.setDefaults(t.TempDir(), "")

	testEnv := env.NewFromMap(map[string]string{})
	resolver := NewShellVariableResolver(testEnv)
	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, []catwalk.Provider{})
	require.NoError(t, err)

	pc, ok := cfg.Providers.Get("qwen36-local")
	require.True(t, ok, "provider must survive: it already has an explicit model and discovery is opted out")
	require.Len(t, pc.Models, 1)
	require.Equal(t, "qwen3.6-30b-instruct", pc.Models[0].ID)
	require.Equal(t, ModelsSourceConfig, pc.ModelsSource)
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
