package config

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// readConfigJSON reads and unmarshals the JSON config file at path.
func readConfigJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	baseDir := filepath.Dir(path)
	fileName := filepath.Base(path)
	b, err := fs.ReadFile(os.DirFS(baseDir), fileName)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

// readRecentModels reads the recent_models list from the config file.
func readRecentModels(t *testing.T, path string) []any {
	t.Helper()
	out := readConfigJSON(t, path)
	rm, ok := out["recent_models"].([]any)
	require.True(t, ok)
	return rm
}

// testStoreWithPath creates a ConfigStore backed by a Config for recent model tests.
func testStoreWithPath(cfg *Config, dir string) *ConfigStore {
	return &ConfigStore{
		config:         cfg,
		globalDataPath: filepath.Join(dir, "config.json"),
	}
}

// configWithRecents builds a Config seeded with the given recent models,
// for exercising the pure nextRecentModels helper.
func configWithRecents(recents ...SelectedModel) *Config {
	return &Config{
		RecentModels: recents,
	}
}

func TestNextRecentModels_AddsToFront(t *testing.T) {
	t.Parallel()

	cfg := configWithRecents()
	updated, changed := nextRecentModels(cfg, SelectedModel{Provider: "openai", Model: "gpt-4o"})
	require.True(t, changed)
	require.Equal(t, []SelectedModel{{Provider: "openai", Model: "gpt-4o"}}, updated)
}

func TestNextRecentModels_DedupeAndMoveToFront(t *testing.T) {
	t.Parallel()

	cfg := configWithRecents(
		SelectedModel{Provider: "anthropic", Model: "claude"},
		SelectedModel{Provider: "openai", Model: "gpt-4o"},
	)
	updated, changed := nextRecentModels(cfg, SelectedModel{Provider: "openai", Model: "gpt-4o"})
	require.True(t, changed)
	require.Equal(t, []SelectedModel{
		{Provider: "openai", Model: "gpt-4o"},
		{Provider: "anthropic", Model: "claude"},
	}, updated)
}

func TestNextRecentModels_TrimsToMax(t *testing.T) {
	t.Parallel()

	var seed []SelectedModel
	for _, id := range []string{"m5", "m4", "m3", "m2", "m1"} {
		seed = append(seed, SelectedModel{Provider: "p", Model: id})
	}
	cfg := configWithRecents(seed...)

	updated, changed := nextRecentModels(cfg, SelectedModel{Provider: "p", Model: "m6"})
	require.True(t, changed)
	require.Len(t, updated, maxRecentModels)
	require.Equal(t, SelectedModel{Provider: "p", Model: "m6"}, updated[0])
	require.Equal(t, SelectedModel{Provider: "p", Model: "m2"}, updated[maxRecentModels-1])
}

func TestNextRecentModels_SkipsEmptyValues(t *testing.T) {
	t.Parallel()

	cfg := configWithRecents()
	_, changed := nextRecentModels(cfg, SelectedModel{Provider: "", Model: "m"})
	require.False(t, changed)
	_, changed = nextRecentModels(cfg, SelectedModel{Provider: "p", Model: ""})
	require.False(t, changed)
}

func TestNextRecentModels_NoChangeWhenAlreadyFront(t *testing.T) {
	t.Parallel()

	entry := SelectedModel{Provider: "openai", Model: "gpt-4o"}
	cfg := configWithRecents(entry)
	_, changed := nextRecentModels(cfg, entry)
	require.False(t, changed)
}

func TestUpdatePreferredModel_PersistsModelAndRecents(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &Config{}
	cfg.setDefaults(dir, "")
	store := testStoreWithPath(cfg, dir)

	sel := SelectedModel{Provider: "openai", Model: "gpt-4o"}
	require.NoError(t, store.UpdatePreferredModel(ScopeGlobal, sel))

	// in-memory state (read through the store; copy-on-write publishes a
	// new Config, so the seed cfg pointer is intentionally unchanged).
	require.Equal(t, sel, store.Config().Model)
	require.Len(t, store.Config().RecentModels, 1)

	// persisted state
	rm := readRecentModels(t, store.globalDataPath)
	require.Len(t, rm, 1)
	item := rm[0].(map[string]any)
	require.Equal(t, "openai", item["provider"])
	require.Equal(t, "gpt-4o", item["model"])
}

func TestUpdatePreferredModel_AddsToRecentsFront(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &Config{}
	cfg.setDefaults(dir, "")
	store := testStoreWithPath(cfg, dir)

	first := SelectedModel{Provider: "openai", Model: "gpt-4o"}
	require.NoError(t, store.UpdatePreferredModel(ScopeGlobal, first))

	second := SelectedModel{Provider: "google", Model: "gemini"}
	require.NoError(t, store.UpdatePreferredModel(ScopeGlobal, second))

	require.Len(t, store.Config().RecentModels, 2)
	require.Equal(t, second, store.Config().RecentModels[0])
	require.Equal(t, first, store.Config().RecentModels[1])
}
