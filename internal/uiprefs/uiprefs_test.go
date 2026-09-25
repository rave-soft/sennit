package uiprefs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// newTestStore points a fresh global config dir and a fresh, trusted
// project directory at each other, and loads a real *config.ConfigStore
// through the production pipeline — the same one DefaultCommon wraps in
// cmd/root.go. It seeds the global layer with a theme and compact mode, and
// the project layer with a scrollbar override, so a test can check that
// both layers still merge the way they did when the UI read them straight
// off Workspace.Config().
func newTestStore(t *testing.T) (*config.ConfigStore, string) {
	t.Helper()

	globalDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDir)

	globalSeed := `{"options":{"tui":{"theme":"dark","compact_mode":false}}}`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "sennit.json"), []byte(globalSeed), 0o644))

	workingDir := t.TempDir()
	projectSeed := `{"options":{"tui":{"scrollbar":"always"}}}`
	require.NoError(t, os.WriteFile(filepath.Join(workingDir, "sennit.json"), []byte(projectSeed), 0o644))
	require.NoError(t, config.Trust(workingDir))

	store, err := config.LoadData(workingDir, "", false)
	require.NoError(t, err)

	return store, globalDir
}

// TestConfigStoreAdapter_PrefsMergesGlobalAndProjectLayers proves Prefs()
// sees exactly what Workspace.Config() saw: the global theme/compact-mode
// values and the project-scoped scrollbar override, merged together.
func TestConfigStoreAdapter_PrefsMergesGlobalAndProjectLayers(t *testing.T) {
	store, _ := newTestStore(t)
	adapter := NewConfigStoreAdapter(store)

	prefs := adapter.Prefs()
	require.Equal(t, "dark", prefs.ThemeID)
	require.False(t, prefs.CompactMode)
	require.Equal(t, "always", prefs.Scrollbar, "project-scoped override must still apply")
}

// TestConfigStoreAdapter_SetWritesGlobalConfigFieldAndReloads is the
// acceptance check: toggling a pref through the adapter writes the same
// config file key the UI wrote before this change, and the next Prefs()
// call reflects it.
func TestConfigStoreAdapter_SetWritesGlobalConfigFieldAndReloads(t *testing.T) {
	store, globalDir := newTestStore(t)
	adapter := NewConfigStoreAdapter(store)

	require.NoError(t, adapter.Set("options.tui.theme", "solarized"))

	raw, err := os.ReadFile(filepath.Join(globalDir, "sennit.json"))
	require.NoError(t, err)
	require.Equal(t, "solarized", gjson.GetBytes(raw, "options.tui.theme").String(),
		"Set must write options.tui.theme in the global config file, the same key SetConfigField wrote")

	require.Equal(t, "solarized", adapter.Prefs().ThemeID, "Prefs() must see the write without a fresh adapter")
}

// TestConfigStoreAdapter_SetCompactModeWritesGlobalConfigFieldAndReloads
// covers the typed setter the same way: same file, same key, the store's
// own SetCompactMode path (not a generic field write).
func TestConfigStoreAdapter_SetCompactModeWritesGlobalConfigFieldAndReloads(t *testing.T) {
	store, globalDir := newTestStore(t)
	adapter := NewConfigStoreAdapter(store)

	require.NoError(t, adapter.SetCompactMode(true))

	raw, err := os.ReadFile(filepath.Join(globalDir, "sennit.json"))
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(raw, "options.tui.compact_mode").Bool(),
		"SetCompactMode must write options.tui.compact_mode in the global config file")

	require.True(t, adapter.Prefs().CompactMode, "Prefs() must see the write without a fresh adapter")
}

func TestFromConfig_Defaults(t *testing.T) {
	prefs := FromConfig(&config.Config{})
	require.Equal(t, "scramble", prefs.SpinnerMode)
	require.True(t, prefs.ProgressEnabled)
	require.Equal(t, "auto", prefs.NotificationStyle)
}

func TestMemStore_SetTracksWritesAndAppliesKnownKeys(t *testing.T) {
	m := &MemStore{}

	require.NoError(t, m.Set("options.tui.theme", "dark"))
	require.NoError(t, m.SetCompactMode(true))

	require.Equal(t, "dark", m.Prefs().ThemeID)
	require.True(t, m.Prefs().CompactMode)
	require.Len(t, m.Sets, 2)
}
