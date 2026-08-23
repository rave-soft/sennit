package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/testenv"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestConfigureProviders_DropsStaleAnthropicOAuthFromUserConfig verifies
// that dropping a stale Anthropic OAuth provider (Claude Code subscription
// support was removed) deletes providers.anthropic from the config file
// that actually declares it, not just from the machine-owned data file
// ConfigPath(ScopeGlobal) resolves to. It also verifies the data file is
// left untouched: a rewrite there (even a no-op sjson.Delete of a key that
// was never present) would bump its mtime on every reload, which is what
// caused sibling instances watching the file to ping-pong reload each
// other.
func TestConfigureProviders_DropsStaleAnthropicOAuthFromUserConfig(t *testing.T) {
	dir := t.TempDir()
	userDir := filepath.Join(dir, "user")
	dataDir := filepath.Join(dir, "data")
	require.NoError(t, os.MkdirAll(userDir, 0o755))
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	// GlobalConfig() (the user's own sennit.json) is where the stale OAuth
	// entry actually lives; GlobalConfigData() is the separate,
	// machine-owned data file ConfigPath(ScopeGlobal) resolves to.
	t.Setenv("SENNIT_GLOBAL_CONFIG", userDir)
	t.Setenv("SENNIT_GLOBAL_DATA", dataDir)

	userConfigPath := GlobalConfig()
	dataConfigPath := GlobalConfigData()

	userConfigContent := `{"providers":{"anthropic":{"api_key":"key","oauth":{"access_token":"a","refresh_token":"r"}}}}`
	require.NoError(t, os.WriteFile(userConfigPath, []byte(userConfigContent), 0o600))
	dataConfigContent := `{}`
	require.NoError(t, os.WriteFile(dataConfigPath, []byte(dataConfigContent), 0o600))

	dataStatBefore, err := os.Stat(dataConfigPath)
	require.NoError(t, err)

	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.Providers.Set("anthropic", ProviderConfig{
		APIKey:     "key",
		OAuthToken: &oauth.Token{AccessToken: "a", RefreshToken: "r"},
	})
	cfg.setDefaults("/tmp", "")

	store := NewTestStore(t, cfg)
	store.globalDataPath = dataConfigPath

	knownProviders := []catwalk.Provider{
		{
			ID:          catwalk.InferenceProviderAnthropic,
			APIKey:      "$ANTHROPIC_API_KEY",
			APIEndpoint: "https://api.anthropic.com/v1",
		},
	}
	env := testenv.New(map[string]string{})
	resolver := NewShellVariableResolver(env)

	err = cfg.configureProviders(context.Background(), store, env, resolver, knownProviders)
	require.NoError(t, err)

	// Dropped from memory.
	_, exists := cfg.Providers.Get("anthropic")
	require.False(t, exists, "stale anthropic OAuth provider should be dropped")

	// Actually removed from the file that declared it.
	userAfter, err := os.ReadFile(userConfigPath)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(userAfter, "providers.anthropic").Exists(),
		"providers.anthropic should be removed from the user's config file")

	// The data file was never rewritten: same content, same mtime.
	dataAfter, err := os.ReadFile(dataConfigPath)
	require.NoError(t, err)
	require.Equal(t, dataConfigContent, string(dataAfter))
	dataStatAfter, err := os.Stat(dataConfigPath)
	require.NoError(t, err)
	require.Equal(t, dataStatBefore.ModTime(), dataStatAfter.ModTime(),
		"data file must not be rewritten when the key it never had is deleted")
}
