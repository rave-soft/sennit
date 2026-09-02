package config

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/state"
)

// fakeCodexJWT builds an unsigned token carrying the given chatgpt_account_id
// claim, the same way internal/oauth/codex's own tests do (see fakeJWT in
// codex_test.go) — nothing here verifies signatures, only that AccountID
// reads the claim back out, so an unsigned token exercises the same path.
func fakeCodexJWT(t *testing.T, accountID string) string {
	t.Helper()
	claims := map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
		"exp":                         time.Now().Add(10 * 24 * time.Hour).Unix(),
	}
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

func newCodexTestStore(t *testing.T) *ConfigStore {
	t.Helper()
	return newCodexTestStoreWithProxy(t, "")
}

// newCodexTestStoreWithProxy seeds the codex provider's proxy_url at
// construction time, rather than a test poking the published Config in
// place afterwards — Config snapshots are meant to be immutable once
// published (mutators clone-and-swap; see the ConfigStore doc comment in
// store.go), so tests should set up state the same way. ProxyURL and
// ConfiguredProxyURL start equal, matching what providerload sets them to
// at load time, before any account has ever overridden the route.
func newCodexTestStoreWithProxy(t *testing.T, proxyURL string) *ConfigStore {
	t.Helper()
	return newCodexTestStoreWithProxies(t, proxyURL, proxyURL)
}

// newCodexTestStoreWithProxies seeds ProxyURL and ConfiguredProxyURL
// independently, for tests that need to simulate a route an earlier
// account switch already moved off of the provider's configured value.
func newCodexTestStoreWithProxies(t *testing.T, proxyURL, configuredProxyURL string) *ConfigStore {
	t.Helper()
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set(codex.ProviderID, ProviderConfig{ID: codex.ProviderID, ProxyURL: configuredProxyURL})
	runtimeProviders := csync.NewMap[string, state.Provider]()
	runtimeProviders.Set(codex.ProviderID, state.Provider{ID: codex.ProviderID, ProxyURL: proxyURL, ConfiguredProxyURL: configuredProxyURL})
	return &ConfigStore{config: &Config{Providers: providers, RuntimeProviders: runtimeProviders}, processor: testRuntimeProcessor{}}
}

// TestUpdateProviderCredentials_CodexRefreshesAccountHeader pins the bug the
// task description calls out: SetupCodex derives the chatgpt-account-id
// header from the token it is given, so publishing a different account's
// credentials must recompute the header, not leave the previous account's
// ID sitting in ExtraHeaders.
func TestUpdateProviderCredentials_CodexRefreshesAccountHeader(t *testing.T) {
	t.Parallel()

	store := newCodexTestStore(t)

	tokenA := fakeCodexJWT(t, "acct-aaa")
	require.NoError(t, store.UpdateProviderCredentials(codex.ProviderID, tokenA, &oauth.Token{AccessToken: tokenA}))
	provider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "acct-aaa", provider.ExtraHeaders["chatgpt-account-id"])

	tokenB := fakeCodexJWT(t, "acct-bbb")
	require.NoError(t, store.UpdateProviderCredentials(codex.ProviderID, tokenB, &oauth.Token{AccessToken: tokenB}))
	provider, ok = store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "acct-bbb", provider.ExtraHeaders["chatgpt-account-id"])
}

// TestUpdateProviderCredentials_CodexClearsHeaderWhenAccountUnclaimed covers
// the symmetric case: switching from an account whose token names one to an
// account whose token doesn't (a personal-plan token, which carries no
// chatgpt_account_id claim) must remove the stale header rather than leave
// the old account's ID in place — maps.Copy alone never deletes.
func TestUpdateProviderCredentials_CodexClearsHeaderWhenAccountUnclaimed(t *testing.T) {
	t.Parallel()

	store := newCodexTestStore(t)

	tokenA := fakeCodexJWT(t, "acct-aaa")
	require.NoError(t, store.UpdateProviderCredentials(codex.ProviderID, tokenA, &oauth.Token{AccessToken: tokenA}))
	provider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "acct-aaa", provider.ExtraHeaders["chatgpt-account-id"])

	unclaimed := "opaque-token-without-claims"
	require.NoError(t, store.UpdateProviderCredentials(codex.ProviderID, unclaimed, &oauth.Token{AccessToken: unclaimed}))
	provider, ok = store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	_, present := provider.ExtraHeaders["chatgpt-account-id"]
	require.False(t, present, "stale chatgpt-account-id header should have been removed")
}

func TestUpdateProviderAccount_NilAccountProxyLeavesRouteUntouched(t *testing.T) {
	t.Parallel()

	store := newCodexTestStoreWithProxy(t, "http://existing:8080")

	before := store.CredentialVersion()
	require.NoError(t, store.UpdateProviderAccount(codex.ProviderID, AccountCredential{APIKey: "key"}))

	provider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "http://existing:8080", provider.ProxyURL)
	require.Equal(t, "http://existing:8080", provider.ConfiguredProxyURL)
	require.Greater(t, store.CredentialVersion(), before)
}

func TestUpdateProviderAccount_AccountProxyOverridesConfigured(t *testing.T) {
	t.Parallel()

	store := newCodexTestStoreWithProxy(t, "http://provider:8080")

	accountProxy := "http://account:9090"
	before := store.CredentialVersion()
	require.NoError(t, store.UpdateProviderAccount(codex.ProviderID, AccountCredential{APIKey: "key", ProxyURL: &accountProxy}))

	provider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "http://account:9090", provider.ProxyURL, "effective proxy should be the account's")
	require.Equal(t, "http://provider:8080", provider.ConfiguredProxyURL, "provider's configured proxy must survive the switch")
	require.Greater(t, store.CredentialVersion(), before)
}

// TestUpdateProviderAccount_SwitchingBackFallsBackToProviderProxy is the
// regression this whole ConfiguredProxyURL split exists for: switching to
// an account with its own proxy and then to one without must return to the
// provider's configured proxy, not silently keep routing through the
// PREVIOUS account's proxy because that's what ProxyURL last held.
func TestUpdateProviderAccount_SwitchingBackFallsBackToProviderProxy(t *testing.T) {
	t.Parallel()

	store := newCodexTestStoreWithProxy(t, "http://provider:8080")

	accountProxy := "http://account:9090"
	require.NoError(t, store.UpdateProviderAccount(codex.ProviderID, AccountCredential{APIKey: "key", ProxyURL: &accountProxy}))
	provider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "http://account:9090", provider.ProxyURL)

	noOwnProxy := ""
	require.NoError(t, store.UpdateProviderAccount(codex.ProviderID, AccountCredential{APIKey: "key", ProxyURL: &noOwnProxy}))
	provider, ok = store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "http://provider:8080", provider.ProxyURL, "must fall back to the provider's proxy, not stay on the old account's")
	require.Equal(t, "http://provider:8080", provider.ConfiguredProxyURL)
}

func TestUpdateProviderAccount_AccountProxyNoneForcesDirect(t *testing.T) {
	t.Parallel()

	store := newCodexTestStoreWithProxy(t, "http://provider:8080")

	none := "none"
	before := store.CredentialVersion()
	require.NoError(t, store.UpdateProviderAccount(codex.ProviderID, AccountCredential{APIKey: "key", ProxyURL: &none}))

	provider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "none", provider.ProxyURL)
	require.Equal(t, "http://provider:8080", provider.ConfiguredProxyURL)
	require.Greater(t, store.CredentialVersion(), before)
}

// TestUpdateProviderAccount_EmptyConfiguredFallsBackToAccountProxy covers a
// provider loaded without ConfiguredProxyURL ever having been set (an old
// load path, or a test that forgot it): the account's own proxy must still
// become effective rather than resolving to emptiness.
func TestUpdateProviderAccount_EmptyConfiguredFallsBackToAccountProxy(t *testing.T) {
	t.Parallel()

	store := newCodexTestStoreWithProxies(t, "", "")

	accountProxy := "http://account:9090"
	require.NoError(t, store.UpdateProviderAccount(codex.ProviderID, AccountCredential{APIKey: "key", ProxyURL: &accountProxy}))

	provider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "http://account:9090", provider.ProxyURL)
}

// TestUpdateProviderCredentials_PublishesToBothProvidersAndRuntime is the
// regression test for the bug where UpdateProviderAccount stopped writing
// the published credential onto cfg.Providers, leaving readers that still
// take the token from there (credentials.Manager's refresh path,
// runtime_builder.go) looking at a stale entry forever — the very next read
// would see the just-refreshed token as still expired. ProxyURL and
// APIKeyTemplate must NOT be mirrored: ProviderConfig has no such fields,
// and an account's effective proxy/template only ever live in the runtime
// view (see reload.go's carry-forward comment).
func TestUpdateProviderCredentials_PublishesToBothProvidersAndRuntime(t *testing.T) {
	t.Parallel()

	store := newCodexTestStoreWithProxy(t, "http://provider:8080")

	newToken := fakeCodexJWT(t, "acct-new")
	accountProxy := "http://account:9090"
	cred := AccountCredential{
		APIKey:          newToken,
		APIKeyTemplate:  "$SHOULD_NOT_LEAK",
		Token:           &oauth.Token{AccessToken: newToken},
		ProxyURL:        &accountProxy,
		ActiveAccountID: "acct-new",
	}
	require.NoError(t, store.UpdateProviderAccount(codex.ProviderID, cred))

	configured, ok := store.Config().Providers.Get(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, newToken, configured.APIKey, "Config().Providers must see the new token")
	require.NotNil(t, configured.OAuthToken)
	require.Equal(t, newToken, configured.OAuthToken.AccessToken)
	require.Equal(t, "acct-new", configured.Account)

	runtime, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, newToken, runtime.APIKey, "RuntimeProvider must see the new token")
	require.NotNil(t, runtime.OAuthToken)
	require.Equal(t, newToken, runtime.OAuthToken.AccessToken)
	require.Equal(t, "acct-new", runtime.Account)
	require.Equal(t, "$SHOULD_NOT_LEAK", runtime.APIKeyTemplate, "runtime side still gets the template")
	require.Equal(t, "http://account:9090", runtime.ProxyURL, "runtime side gets the account's effective proxy")

	require.Equal(t, "http://provider:8080", configured.ProxyURL,
		"the account's proxy override must not leak into Providers — it is a runtime-only concept")
}

func TestUpdateProviderAccount_CopilotPathUnaffected(t *testing.T) {
	t.Parallel()

	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set(string(catwalk.InferenceProviderCopilot), ProviderConfig{ID: string(catwalk.InferenceProviderCopilot)})
	store := &ConfigStore{config: &Config{Providers: providers}, processor: testRuntimeProcessor{}}

	before := store.CredentialVersion()
	require.NoError(t, store.UpdateProviderCredentials(string(catwalk.InferenceProviderCopilot), "key", nil))

	provider, ok := store.Config().RuntimeProvider(string(catwalk.InferenceProviderCopilot))
	require.True(t, ok)
	require.NotEmpty(t, provider.ExtraHeaders, "Copilot headers should be present")
	require.Greater(t, store.CredentialVersion(), before)
}
