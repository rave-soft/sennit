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
// store.go), so tests should set up state the same way.
func newCodexTestStoreWithProxy(t *testing.T, proxyURL string) *ConfigStore {
	t.Helper()
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set(codex.ProviderID, ProviderConfig{ID: codex.ProviderID, ProxyURL: proxyURL})
	return &ConfigStore{config: &Config{Providers: providers}}
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
	provider, ok := store.Config().Providers.Get(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "acct-aaa", provider.ExtraHeaders["chatgpt-account-id"])

	tokenB := fakeCodexJWT(t, "acct-bbb")
	require.NoError(t, store.UpdateProviderCredentials(codex.ProviderID, tokenB, &oauth.Token{AccessToken: tokenB}))
	provider, ok = store.Config().Providers.Get(codex.ProviderID)
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
	provider, ok := store.Config().Providers.Get(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "acct-aaa", provider.ExtraHeaders["chatgpt-account-id"])

	unclaimed := "opaque-token-without-claims"
	require.NoError(t, store.UpdateProviderCredentials(codex.ProviderID, unclaimed, &oauth.Token{AccessToken: unclaimed}))
	provider, ok = store.Config().Providers.Get(codex.ProviderID)
	require.True(t, ok)
	_, present := provider.ExtraHeaders["chatgpt-account-id"]
	require.False(t, present, "stale chatgpt-account-id header should have been removed")
}

func TestUpdateProviderAccount_NilProxyLeavesProviderProxyUntouched(t *testing.T) {
	t.Parallel()

	store := newCodexTestStoreWithProxy(t, "http://existing:8080")

	before := store.CredentialVersion()
	require.NoError(t, store.UpdateProviderAccount(codex.ProviderID, "key", nil, nil))

	provider, ok := store.Config().Providers.Get(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "http://existing:8080", provider.ProxyURL)
	require.Greater(t, store.CredentialVersion(), before)
}

func TestUpdateProviderAccount_EmptyPointerClearsProxy(t *testing.T) {
	t.Parallel()

	store := newCodexTestStoreWithProxy(t, "http://existing:8080")

	empty := ""
	before := store.CredentialVersion()
	require.NoError(t, store.UpdateProviderAccount(codex.ProviderID, "key", nil, &empty))

	provider, ok := store.Config().Providers.Get(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "", provider.ProxyURL)
	require.Greater(t, store.CredentialVersion(), before)
}

func TestUpdateProviderAccount_NoneSetsDirect(t *testing.T) {
	t.Parallel()

	store := newCodexTestStore(t)

	none := "none"
	before := store.CredentialVersion()
	require.NoError(t, store.UpdateProviderAccount(codex.ProviderID, "key", nil, &none))

	provider, ok := store.Config().Providers.Get(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, "none", provider.ProxyURL)
	require.Greater(t, store.CredentialVersion(), before)
}

func TestUpdateProviderAccount_CopilotPathUnaffected(t *testing.T) {
	t.Parallel()

	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set(string(catwalk.InferenceProviderCopilot), ProviderConfig{ID: string(catwalk.InferenceProviderCopilot)})
	store := &ConfigStore{config: &Config{Providers: providers}}

	before := store.CredentialVersion()
	require.NoError(t, store.UpdateProviderCredentials(string(catwalk.InferenceProviderCopilot), "key", nil))

	provider, ok := store.Config().Providers.Get(string(catwalk.InferenceProviderCopilot))
	require.True(t, ok)
	require.NotEmpty(t, provider.ExtraHeaders, "Copilot headers should be present")
	require.Greater(t, store.CredentialVersion(), before)
}
