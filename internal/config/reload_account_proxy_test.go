package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/accounts"
)

// TestReloadFromDisk_PreservesAccountProxyOnUnrelatedReload guards against
// a bug where an active account's resolved ProxyURL/APIKeyTemplate only
// survived a reload that raced a concurrent credential change (see
// TestReloadFromDisk_CredentialRacePreservesFullAccountSwitch). Any other
// reload — the file watcher, SetConfigField on an unrelated key,
// EnableDockerMCP, EnsureAccountMigrated — never bumps credentialVersion,
// so buildConfig's fresh disk read (providerload/loader.go rebuilds
// ProxyURL from the provider's own proxy_url) silently replaced the
// account's socks5/none override with the provider default, even though
// the account stayed "active".
func TestReloadFromDisk_PreservesAccountProxyOnUnrelatedReload(t *testing.T) {
	globalDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDir)

	token := fakeCodexJWT(t, "acct-only")
	seed := fmt.Sprintf(`{
  "options": {"disable_default_providers": true},
  "providers": {
    "mock": {"id": "mock", "name": "Mock", "type": "openai",
      "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
      "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192}]},
    "codex": {"id": "codex", "name": "Codex", "type": "openai",
      "base_url": "http://127.0.0.1:9/v1", "api_key": %q,
      "oauth": {"access_token": %q}, "proxy_url": "http://provider-proxy:8080",
      "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192}]}
  },
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`, token, token)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, appName+".json"), []byte(seed), 0o644))

	workingDir := t.TempDir()
	store, err := LoadData(workingDir, "", false)
	require.NoError(t, err)

	account := accounts.Account{
		ID:       "acct-only",
		Token:    &oauth.Token{AccessToken: token},
		ProxyURL: "socks5://account-proxy:1080",
	}
	require.NoError(t, store.ActivateAccount(ScopeGlobal, codex.ProviderID, account))

	provider, ok := store.Config().Providers.Get(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "socks5://account-proxy:1080", provider.ProxyURL, "sanity: account proxy published")

	// A plain reload, unrelated to any credential change (nothing bumps
	// credentialVersion between here and the reload below).
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	provider, ok = store.Config().Providers.Get(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "acct-only", provider.Account, "account must still be active")
	require.Equal(t, "socks5://account-proxy:1080", provider.ProxyURL,
		"the account's proxy override must survive a reload not caused by a credential change")
}
