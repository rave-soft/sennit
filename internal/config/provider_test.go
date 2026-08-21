package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProviders_PerConfigDisableDefaultProviders: Providers used to
// memoize its result process-globally via sync.Once, so the first *Config
// it ever saw fixed the answer for every
// other *Config passed in afterwards. In a multi-workspace process, the
// first workspace loaded would silently decide DisableDefaultProviders for
// every other workspace. Providers must be a pure function of its argument.
func TestProviders_PerConfigDisableDefaultProviders(t *testing.T) {
	enabled := &Config{Options: &Options{}}
	disabled := &Config{Options: &Options{DisableDefaultProviders: true}}

	// Call in this order so a cache primed by the first call would leak
	// into the second: with a process-global cache, "disabled" would
	// incorrectly see the embedded catalog "enabled" produced.
	got := Providers(enabled)
	require.NotEmpty(t, got, "embedded catalog should load when defaults are enabled")

	got = Providers(disabled)
	require.Empty(t, got, "the embedded catalog must not leak into a config with defaults disabled")
}

// TestConfigStore_KnownProvidersPerStore covers the same bug at the
// ConfigStore level: two stores loaded in the same process with different
// DisableDefaultProviders settings must each keep their own catalog rather
// than sharing whichever one loaded first.
func TestConfigStore_KnownProvidersPerStore(t *testing.T) {
	dirEnabled := t.TempDir()
	dirDisabled := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dirEnabled, "sennit.json"), []byte(`{}`), 0o600))
	// Load requires at least one custom provider when defaults are
	// disabled, so this config supplies one purely to satisfy that
	// bootstrap check; the test itself only cares about KnownProviders().
	disabledConfig := `{
		"options": {"disable_default_providers": true},
		"providers": {
			"my-custom": {
				"type": "openai-compat",
				"base_url": "https://example.com/v1",
				"name": "My Custom",
				"models": [{"id": "custom-model", "name": "Custom Model"}]
			}
		}
	}`
	require.NoError(t, os.WriteFile(filepath.Join(dirDisabled, "sennit.json"), []byte(disabledConfig), 0o600))

	t.Setenv("SENNIT_GLOBAL_CONFIG", dirEnabled)
	t.Setenv("SENNIT_GLOBAL_DATA", dirEnabled)

	storeEnabled, err := Load(dirEnabled, dirEnabled, false)
	require.NoError(t, err)
	require.NotEmpty(t, storeEnabled.KnownProviders(), "embedded catalog should load for the first store")

	t.Setenv("SENNIT_GLOBAL_CONFIG", dirDisabled)
	t.Setenv("SENNIT_GLOBAL_DATA", dirDisabled)

	storeDisabled, err := Load(dirDisabled, dirDisabled, false)
	require.NoError(t, err)
	require.Empty(t, storeDisabled.KnownProviders(), "the second store's DisableDefaultProviders must not be overridden by the first store's catalog")

	// The first store must be unaffected by loading the second.
	require.NotEmpty(t, storeEnabled.KnownProviders(), "the first store's catalog must survive loading a second store")
}
