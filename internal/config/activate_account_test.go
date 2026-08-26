package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/env"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/accounts"
)

// newActivateAccountTestStore builds a store whose codex provider has
// providerProxy as its configured proxy, wired with a real shell resolver
// (env.New(), same as docker_mcp_test.go) so $VAR-style api_key templates
// actually expand.
func newActivateAccountTestStore(t *testing.T, providerProxy string) *ConfigStore {
	t.Helper()
	dir := t.TempDir()
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set(codex.ProviderID, ProviderConfig{
		ID:                 codex.ProviderID,
		ProxyURL:           providerProxy,
		ConfiguredProxyURL: providerProxy,
	})
	return &ConfigStore{
		config:         &Config{Providers: providers},
		globalDataPath: filepath.Join(dir, "sennit.json"),
		resolver:       NewShellVariableResolver(env.New()),
	}
}

func TestActivateAccount_OAuthAccountPublishesTokenAndPersistsChoice(t *testing.T) {
	t.Parallel()

	store := newActivateAccountTestStore(t, "")
	before := store.CredentialVersion()

	token := fakeCodexJWT(t, "acct-new")
	acct := accounts.Account{ID: "acct-1", Token: &oauth.Token{AccessToken: token}}
	require.NoError(t, store.ActivateAccount(ScopeGlobal, codex.ProviderID, acct))

	provider, ok := store.Config().Providers.Get(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, token, provider.APIKey)
	require.Equal(t, token, provider.OAuthToken.AccessToken)
	require.Equal(t, "acct-new", provider.ExtraHeaders["chatgpt-account-id"])
	require.Greater(t, store.CredentialVersion(), before)

	data, err := os.ReadFile(store.globalDataPath)
	require.NoError(t, err)
	require.Equal(t, "acct-1", gjson.GetBytes(data, "providers.codex.account").String())
}

func TestActivateAccount_APIKeyAccountPublishesResolvedValue(t *testing.T) {
	t.Setenv("SENNIT_TEST_ACCOUNT_KEY", "resolved-secret")
	store := newActivateAccountTestStore(t, "")

	acct := accounts.Account{ID: "acct-2", APIKey: "$SENNIT_TEST_ACCOUNT_KEY"}
	require.NoError(t, store.ActivateAccount(ScopeGlobal, codex.ProviderID, acct))

	provider, ok := store.Config().Providers.Get(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "resolved-secret", provider.APIKey, "the resolved value must go out on the wire, not the template")
	require.Equal(t, "$SENNIT_TEST_ACCOUNT_KEY", provider.APIKeyTemplate)
}

func TestActivateAccount_UnresolvableAPIKeyPublishesNothing(t *testing.T) {
	store := newActivateAccountTestStore(t, "")
	before := store.CredentialVersion()

	acct := accounts.Account{ID: "acct-3", APIKey: "$SENNIT_TEST_ACCOUNT_KEY_UNSET_XYZ"}
	err := store.ActivateAccount(ScopeGlobal, codex.ProviderID, acct)
	require.Error(t, err)
	require.Equal(t, before, store.CredentialVersion())

	provider, ok := store.Config().Providers.Get(codex.ProviderID)
	require.True(t, ok)
	require.Empty(t, provider.APIKey)
}

func TestActivateAccount_SwitchingToAccountWithoutProxyFallsBackToProvider(t *testing.T) {
	store := newActivateAccountTestStore(t, "http://provider:8080")

	withProxy := accounts.Account{ID: "acct-a", APIKey: "$SENNIT_TEST_ACCOUNT_KEY_A", ProxyURL: "http://account:9090"}
	t.Setenv("SENNIT_TEST_ACCOUNT_KEY_A", "a-secret")
	require.NoError(t, store.ActivateAccount(ScopeGlobal, codex.ProviderID, withProxy))
	provider, ok := store.Config().Providers.Get(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "http://account:9090", provider.ProxyURL)

	withoutProxy := accounts.Account{ID: "acct-b", APIKey: "$SENNIT_TEST_ACCOUNT_KEY_B"}
	t.Setenv("SENNIT_TEST_ACCOUNT_KEY_B", "b-secret")
	require.NoError(t, store.ActivateAccount(ScopeGlobal, codex.ProviderID, withoutProxy))
	provider, ok = store.Config().Providers.Get(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "http://provider:8080", provider.ProxyURL, "must fall back to the provider's proxy, not the previous account's")
}

func TestActivateAccount_InvalidAccountPublishesNothing(t *testing.T) {
	t.Parallel()

	store := newActivateAccountTestStore(t, "")
	before := store.CredentialVersion()

	err := store.ActivateAccount(ScopeGlobal, codex.ProviderID, accounts.Account{})
	require.Error(t, err)
	require.Equal(t, before, store.CredentialVersion())

	err = store.ActivateAccount(ScopeGlobal, codex.ProviderID, accounts.Account{
		ID:     "acct-both",
		APIKey: "key",
		Token:  &oauth.Token{AccessToken: "token"},
	})
	require.Error(t, err)
	require.Equal(t, before, store.CredentialVersion())
}
