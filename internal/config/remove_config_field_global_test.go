package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestSetProviderProxy_ClearsFieldFromConfigLayerNotJustDataFile is the
// regression test for "clear the provider proxy" being a silent no-op when
// proxy_url lives in ~/.config/sennit/sennit.json (the GlobalConfig()
// layer) rather than the data file: RemoveConfigField used to resolve
// ScopeGlobal to the data file alone, so a proxy set by hand in the
// documented config layer survived a clear, and the very next reload (which
// SetProviderProxy itself triggers) read it straight back.
//
// SENNIT_GLOBAL_CONFIG and SENNIT_GLOBAL_DATA are pointed at different
// directories so the two global layers really are two different files, not
// the same path under two names.
func TestSetProviderProxy_ClearsFieldFromConfigLayerNotJustDataFile(t *testing.T) {
	configDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", configDir)
	t.Setenv("SENNIT_GLOBAL_DATA", dataDir)

	// The base provider/model definitions live in the data file, same shape
	// as every other mock-provider test fixture (see AGENTS.md); proxy_url
	// lives only in the ~/.config/sennit/sennit.json layer, the documented
	// place a user would hand-edit it.
	dataConfig := `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`
	require.NoError(t, os.WriteFile(GlobalConfigData(), []byte(dataConfig), 0o644))

	require.NoError(t, os.MkdirAll(filepath.Dir(GlobalConfig()), 0o755))
	userConfig := `{"providers":{"mock":{"proxy_url":"http://stale-proxy:8080"}}}`
	require.NoError(t, os.WriteFile(GlobalConfig(), []byte(userConfig), 0o644))

	// A sennitrc sibling in the same directory as the config layer: it must
	// survive completely untouched. RemoveConfigField's guard depends on
	// gjson never matching a key inside a shell script, so this is the case
	// that would silently corrupt it if that guard were missing.
	sennitrcPath := shellConfigSibling(GlobalConfig())
	sennitrcContents := []byte("option debug true\n")
	require.NoError(t, os.WriteFile(sennitrcPath, sennitrcContents, 0o644))

	workingDir := t.TempDir()
	store, err := LoadData(workingDir, "", false)
	require.NoError(t, err)

	// LoadData (no RuntimeProcessor) never runs providerload, which is
	// what populates ConfiguredProxyURL — so the plain JSON field ProxyURL
	// is what reflects the merged, on-disk value here.
	pc, ok := store.Config().Providers.Get("mock")
	require.True(t, ok)
	require.Equal(t, "http://stale-proxy:8080", pc.ProxyURL, "sanity: the proxy was loaded from the config layer")

	accStore := newTestAccountsStore(t)
	require.NoError(t, SetProviderProxy(store, accStore, "mock", ""))

	// The config layer's copy of the key must actually be gone on disk, not
	// just shadowed by an unmodified data file.
	configBytes, err := os.ReadFile(GlobalConfig())
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(configBytes, "providers.mock.proxy_url").Exists(),
		"clearing the proxy must delete the key from the config layer it actually lives in")

	// The sennitrc sibling must be byte-for-byte untouched.
	sennitrcAfter, err := os.ReadFile(sennitrcPath)
	require.NoError(t, err)
	require.Equal(t, sennitrcContents, sennitrcAfter, "the sennitrc sibling must never be rewritten by RemoveConfigField")

	pc, ok = store.Config().Providers.Get("mock")
	require.True(t, ok)
	require.Empty(t, pc.ProxyURL, "the reloaded in-memory config must no longer report the cleared proxy")
}

// TestRemoveConfigField_GlobalScopeNoLayerHasKeySkipsReload guards the
// "only reload when a layer actually changed" claim RemoveConfigField's
// global branch makes: configFile.atomicWrite returns nil (not an error)
// for errAtomicWriteNoop, so a naive "err == nil means wrote" check would
// count a no-op layer as a write and fire autoReload anyway, even though
// every layer's atomicWrite call took the noop branch. wrote must instead
// be driven by whether any layer's mutation closure actually produced new
// bytes.
func TestRemoveConfigField_GlobalScopeNoLayerHasKeySkipsReload(t *testing.T) {
	configDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", configDir)
	t.Setenv("SENNIT_GLOBAL_DATA", dataDir)

	dataConfig := `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`
	require.NoError(t, os.WriteFile(GlobalConfigData(), []byte(dataConfig), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Dir(GlobalConfig()), 0o755))
	require.NoError(t, os.WriteFile(GlobalConfig(), []byte(`{}`), 0o644))

	workingDir := t.TempDir()
	store, err := LoadData(workingDir, "", false)
	require.NoError(t, err)

	beforeData, err := os.Stat(GlobalConfigData())
	require.NoError(t, err)
	beforeConfig, err := os.Stat(GlobalConfig())
	require.NoError(t, err)
	beforeVersion := store.Version()

	require.NoError(t, store.RemoveConfigField(ScopeGlobal, "providers.mock.proxy_url"))

	afterData, err := os.Stat(GlobalConfigData())
	require.NoError(t, err)
	afterConfig, err := os.Stat(GlobalConfig())
	require.NoError(t, err)
	require.Equal(t, beforeData.ModTime(), afterData.ModTime(), "no layer had the key, so the data file must not be rewritten")
	require.Equal(t, beforeConfig.ModTime(), afterConfig.ModTime(), "no layer had the key, so the config file must not be rewritten")
	require.Equal(t, beforeVersion, store.Version(), "no layer changed, so no autoReload should have published a new config")
}
