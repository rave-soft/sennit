package config

import (
	"fmt"
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
	"github.com/rave-soft/sennit/internal/providers/state"
)

// newActivateAccountTestStore builds a store whose codex provider has
// providerProxy as its configured proxy, wired with a real shell resolver
// (env.New(), same as docker_mcp_test.go) so $VAR-style api_key templates
// actually expand.
func newActivateAccountTestStore(t *testing.T, providerProxy string) *ConfigStore {
	t.Helper()
	dir := t.TempDir()
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set(codex.ProviderID, ProviderConfig{ID: codex.ProviderID, ProxyURL: providerProxy})
	runtimeProviders := csync.NewMap[string, state.Provider]()
	runtimeProviders.Set(codex.ProviderID, state.Provider{ID: codex.ProviderID, ProxyURL: providerProxy, ConfiguredProxyURL: providerProxy})
	return &ConfigStore{
		config:         &Config{Providers: providers, RuntimeProviders: runtimeProviders},
		globalDataPath: filepath.Join(dir, "sennit.json"),
		resolver:       NewShellVariableResolver(env.New()),
		processor:      testRuntimeProcessor{},
	}
}

func TestActivateAccount_OAuthAccountPublishesTokenAndPersistsChoice(t *testing.T) {
	t.Parallel()

	store := newActivateAccountTestStore(t, "")
	before := store.CredentialVersion()

	token := fakeCodexJWT(t, "acct-new")
	acct := accounts.Account{ID: "acct-1", Token: &oauth.Token{AccessToken: token}}
	require.NoError(t, store.ActivateAccount(ScopeGlobal, codex.ProviderID, acct))

	provider, ok := store.Config().RuntimeProvider(codex.ProviderID)
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

	provider, ok := store.Config().RuntimeProvider(codex.ProviderID)
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

	provider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Empty(t, provider.APIKey)
}

func TestActivateAccount_SwitchingToAccountWithoutProxyFallsBackToProvider(t *testing.T) {
	store := newActivateAccountTestStore(t, "http://provider:8080")

	withProxy := accounts.Account{ID: "acct-a", APIKey: "$SENNIT_TEST_ACCOUNT_KEY_A", ProxyURL: "http://account:9090"}
	t.Setenv("SENNIT_TEST_ACCOUNT_KEY_A", "a-secret")
	require.NoError(t, store.ActivateAccount(ScopeGlobal, codex.ProviderID, withProxy))
	provider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "http://account:9090", provider.ProxyURL)

	withoutProxy := accounts.Account{ID: "acct-b", APIKey: "$SENNIT_TEST_ACCOUNT_KEY_B"}
	t.Setenv("SENNIT_TEST_ACCOUNT_KEY_B", "b-secret")
	require.NoError(t, store.ActivateAccount(ScopeGlobal, codex.ProviderID, withoutProxy))
	provider, ok = store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "http://provider:8080", provider.ProxyURL, "must fall back to the provider's proxy, not the previous account's")
}

// newLoadedActivateAccountStore builds a real, disk-backed ConfigStore via
// LoadData (not the hand-built stores above) so autoReload actually runs —
// the thing the hand-built-store tests above never exercise. The global
// config seeds a codex provider entry with oldToken as its oauth
// credential, "disable_default_providers" so nothing else in the catalog
// gets pulled in, and a "mock" provider so the model list required by
// config validation is satisfied without touching a real endpoint.
func newLoadedActivateAccountStore(t *testing.T, globalDir string, oldToken string) *ConfigStore {
	t.Helper()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDir)

	seed := fmt.Sprintf(`{
  "options": {"disable_default_providers": true},
  "providers": {
    "mock": {"id": "mock", "name": "Mock", "type": "openai",
      "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
      "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192}]},
    "codex": {"id": "codex", "name": "Codex", "type": "openai",
      "base_url": "http://127.0.0.1:9/v1", "api_key": %q,
      "oauth": {"access_token": %q},
      "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192}]}
  },
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`, oldToken, oldToken)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, appName+".json"), []byte(seed), 0o644))

	workingDir := t.TempDir()
	store, err := loadRuntimeForTest(workingDir, "", false)
	require.NoError(t, err)
	return store
}

// TestActivateAccount_OAuthSwitchPersistsToDiskAndMemory is the regression
// test for the bug where ActivateAccount published the new account's
// credentials to memory before persisting them to disk: SetConfigFields'
// own reload then rebuilt ProviderConfig from disk, which still had the
// OLD credentials, silently reverting the switch in memory while the
// "account" pointer on disk kept pointing at the new account.
func TestActivateAccount_OAuthSwitchPersistsToDiskAndMemory(t *testing.T) {
	globalDir := t.TempDir()
	oldToken := fakeCodexJWT(t, "acct-old")
	store := newLoadedActivateAccountStore(t, globalDir, oldToken)

	newToken := fakeCodexJWT(t, "acct-new")
	acct := accounts.Account{ID: "acct-new", Token: &oauth.Token{AccessToken: newToken}}
	require.NoError(t, store.ActivateAccount(ScopeGlobal, codex.ProviderID, acct))

	provider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, newToken, provider.APIKey, "the new account's token must survive the reload")
	require.Equal(t, newToken, provider.OAuthToken.AccessToken)

	data := requireFile(t, store.globalDataPath)
	require.Equal(t, newToken, gjson.GetBytes(data, "providers.codex.api_key").String(), "the new token must be on disk, not just in memory")
	require.Equal(t, newToken, gjson.GetBytes(data, "providers.codex.oauth.access_token").String())
	require.Equal(t, "acct-new", gjson.GetBytes(data, "providers.codex.account").String())

	// A fresh process (a second store built from the same directory)
	// must come back to the new account, not the old one — this is the
	// exact scenario the bug broke, since disk never actually got the
	// new credentials before.
	restarted, err := loadRuntimeForTest(t.TempDir(), "", false)
	require.NoError(t, err)
	restartedProvider, ok := restarted.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, newToken, restartedProvider.APIKey, "a restarted process must see the new account's credentials")
}

// TestActivateAccount_APIKeySwitchWritesTemplateNotResolvedSecret ensures
// the disk write carries the unresolved api_key template (what the user
// configured), never the resolved secret it expands to, while memory gets
// the resolved value.
func TestActivateAccount_APIKeySwitchWritesTemplateNotResolvedSecret(t *testing.T) {
	globalDir := t.TempDir()
	oldToken := fakeCodexJWT(t, "acct-old")
	t.Setenv("MY_TEST_VAR", "the-resolved-secret")
	store := newLoadedActivateAccountStore(t, globalDir, oldToken)

	acct := accounts.Account{ID: "acct-key", APIKey: "$MY_TEST_VAR"}
	require.NoError(t, store.ActivateAccount(ScopeGlobal, codex.ProviderID, acct))

	provider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "the-resolved-secret", provider.APIKey, "memory must carry the resolved value")

	data := requireFile(t, store.globalDataPath)
	require.Equal(t, "$MY_TEST_VAR", gjson.GetBytes(data, "providers.codex.api_key").String(), "disk must carry the raw template, never the resolved secret")
}

// TestActivateAccount_ProxySurvivesReload ensures the account's effective
// proxy (memory-only, never written to disk) survives SetConfigFields'
// reload, which only knows about disk state.
func TestActivateAccount_ProxySurvivesReload(t *testing.T) {
	globalDir := t.TempDir()
	oldToken := fakeCodexJWT(t, "acct-old")
	t.Setenv("MY_TEST_VAR2", "another-secret")
	store := newLoadedActivateAccountStore(t, globalDir, oldToken)

	acct := accounts.Account{ID: "acct-proxy", APIKey: "$MY_TEST_VAR2", ProxyURL: "http://account-proxy:9090"}
	require.NoError(t, store.ActivateAccount(ScopeGlobal, codex.ProviderID, acct))

	provider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "http://account-proxy:9090", provider.ProxyURL, "the account's proxy override must survive the reload")

	data := requireFile(t, store.globalDataPath)
	require.False(t, gjson.GetBytes(data, "providers.codex.proxy_url").Exists(), "the account's proxy override must never be written to disk")
}

// TestActivateAccount_HandBuiltStorePublishesActiveAccountToMemory is the
// regression test for the bug where UpdateProviderAccount never populated
// ProviderConfig.Account in memory, only on disk: a hand-built store (no
// workingDir, so autoReload never runs — see newActivateAccountTestStore)
// must still see the active account ID in Config() immediately after
// ActivateAccount returns, with no reload in between.
func TestActivateAccount_HandBuiltStorePublishesActiveAccountToMemory(t *testing.T) {
	t.Parallel()

	store := newActivateAccountTestStore(t, "")
	token := fakeCodexJWT(t, "acct-mem")
	acct := accounts.Account{ID: "acct-mem-id", Token: &oauth.Token{AccessToken: token}}
	require.NoError(t, store.ActivateAccount(ScopeGlobal, codex.ProviderID, acct))

	provider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "acct-mem-id", provider.Account, "the active account ID must be published to memory, not just disk")
}

// TestActivateAccount_LoadedStorePublishesActiveAccountToMemoryAndDisk
// covers the same guarantee on a real, disk-backed store (LoadData),
// asserting the active account ID lands in memory immediately AND on
// disk, without relying on autoReload's timing.
func TestActivateAccount_LoadedStorePublishesActiveAccountToMemoryAndDisk(t *testing.T) {
	globalDir := t.TempDir()
	oldToken := fakeCodexJWT(t, "acct-old")
	store := newLoadedActivateAccountStore(t, globalDir, oldToken)

	newToken := fakeCodexJWT(t, "acct-new")
	acct := accounts.Account{ID: "acct-loaded-id", Token: &oauth.Token{AccessToken: newToken}}
	require.NoError(t, store.ActivateAccount(ScopeGlobal, codex.ProviderID, acct))

	provider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "acct-loaded-id", provider.Account, "the active account ID must be in memory immediately, with no wait")

	data := requireFile(t, store.globalDataPath)
	require.Equal(t, "acct-loaded-id", gjson.GetBytes(data, "providers.codex.account").String())
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
