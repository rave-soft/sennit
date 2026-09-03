package appws

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/stretchr/testify/require"
)

// fakeCodexJWT mirrors internal/oauth/codex/codex_test.go's fakeJWT: an
// unsigned token carrying the chatgpt_account_id claim AccountID reads, and
// an expiry codex.Usable checks.
func fakeCodexJWT(t *testing.T, accountID string, life time.Duration) string {
	t.Helper()
	claims := map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
		"exp":                         time.Now().Add(life).Unix(),
	}
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

// newOAuthTestWorkspace builds a real AppWorkspace the way
// credentials_singleton_test.go does: RecordAccount (used by
// CompleteOAuth) reaches through app.Credentials() to signal completion,
// which panics on a zero *app.App, so a fully-wired one is needed rather
// than the lighter &AppWorkspace{app: &app.App{}, store: ...} stand-in
// used elsewhere in this package for read-only methods.
func newOAuthTestWorkspace(t *testing.T) *AppWorkspace {
	t.Helper()
	globalConfigDir := t.TempDir()
	globalDataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalConfigDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDataDir)
	require.NoError(t, os.WriteFile(filepath.Join(globalDataDir, "sennit.json"), []byte("{}"), 0o600))

	store, err := configruntime.Load(t.TempDir(), t.TempDir(), false)
	require.NoError(t, err)

	a := app.NewForTest(t.Context())
	a.SetConfigForTest(store)
	t.Cleanup(a.ShutdownForTest)

	return NewAppWorkspace(a, store)
}

// TestAppWorkspace_StartOAuthCodex_ReusesDiskLogin covers the disk
// short-circuit: an existing Codex CLI login with a still-usable access
// token is returned as a won Token, with no flow to wait on.
func TestAppWorkspace_StartOAuthCodex_ReusesDiskLogin(t *testing.T) {
	// No t.Parallel: t.Setenv pins CODEX_HOME for this test.
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	accessToken := fakeCodexJWT(t, "acct-disk-1", 10*24*time.Hour)
	auth := map[string]any{
		"tokens": map[string]any{
			"access_token":  accessToken,
			"refresh_token": "rt-disk",
		},
	}
	data, err := json.Marshal(auth)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, "auth.json"), data, 0o600))

	w := newOAuthTestWorkspace(t)
	result, flow, err := w.StartOAuth(t.Context(), codex.ProviderID, "")
	require.NoError(t, err)
	require.Nil(t, flow)
	require.NotNil(t, result.Token)
	require.Equal(t, accessToken, result.Token.AccessToken)
	require.True(t, result.ReusedExistingLogin)
	require.False(t, result.RefreshedExistingLogin)
	require.Empty(t, result.AuthorizationURL)
}

// TestAppWorkspace_StartOAuthCodex_FallsBackToBrowserFlow covers the other
// half: nothing on disk starts the loopback listener and returns its URL
// instead of a token.
func TestAppWorkspace_StartOAuthCodex_FallsBackToBrowserFlow(t *testing.T) {
	// No t.Parallel: t.Setenv pins CODEX_HOME for this test.
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	w := newOAuthTestWorkspace(t)
	result, flow, err := w.StartOAuth(t.Context(), codex.ProviderID, "")
	require.NoError(t, err)
	require.NotNil(t, flow)
	t.Cleanup(flow.Cancel)

	require.Nil(t, result.Token)
	require.NotEmpty(t, result.AuthorizationURL)
	require.False(t, result.ReusedExistingLogin)
	require.False(t, result.RefreshedExistingLogin)

	// Do not Wait() on the flow: nothing will ever complete the browser
	// half in this test, and doing so would hang.
}

// newOAuthProxyTestWorkspace mirrors newOAuthTestWorkspace but loads
// through config.LoadData instead of configruntime.Load, i.e. without
// providerload's RuntimeProcessor. The OAuthConfiguredProxy tests below
// only ever read cfg.Providers.Get(id).ProxyURL, so running the full
// catalog validation pipeline would be pure friction: it drops a
// proxy-only Codex/Copilot entry (standing in for "the user set a proxy
// before ever logging in") for having neither credentials nor models,
// which has nothing to do with what these tests check.
func newOAuthProxyTestWorkspace(t *testing.T) *AppWorkspace {
	t.Helper()
	globalConfigDir := t.TempDir()
	globalDataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalConfigDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDataDir)
	require.NoError(t, os.WriteFile(filepath.Join(globalDataDir, "sennit.json"), []byte("{}"), 0o600))

	store, err := config.LoadData(t.TempDir(), t.TempDir(), false)
	require.NoError(t, err)

	a := app.NewForTest(t.Context())
	a.SetConfigForTest(store)
	t.Cleanup(a.ShutdownForTest)

	return NewAppWorkspace(a, store)
}

// TestAppWorkspace_OAuthConfiguredProxyCodex_FallsBackToDisk pins the
// prefill order the dialog used to compute itself: Sennit's own config
// first, then the Codex CLI's on-disk config.
func TestAppWorkspace_OAuthConfiguredProxyCodex_FallsBackToDisk(t *testing.T) {
	// No t.Parallel: t.Setenv pins CODEX_HOME for this test.
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[network]\nproxy_url = \"http://127.0.0.1:8080\"\n"), 0o600))

	w := newOAuthProxyTestWorkspace(t)
	require.Equal(t, "http://127.0.0.1:8080", w.OAuthConfiguredProxy(codex.ProviderID))

	require.NoError(t, w.store.SetConfigField(config.ScopeGlobal, config.ProviderFieldKey(codex.ProviderID, "proxy_url"), "socks5://configured:1080"))
	require.Equal(t, "socks5://configured:1080", w.OAuthConfiguredProxy(codex.ProviderID),
		"Sennit's own configured proxy must win over the CLI's on-disk one")
}

// TestAppWorkspace_OAuthConfiguredProxyCopilot_NoDiskFallback pins that
// Copilot, unlike Codex, has no sibling CLI config to fall back to.
func TestAppWorkspace_OAuthConfiguredProxyCopilot_NoDiskFallback(t *testing.T) {
	w := newOAuthProxyTestWorkspace(t)
	require.Empty(t, w.OAuthConfiguredProxy(copilotProviderID))

	require.NoError(t, w.store.SetConfigField(config.ScopeGlobal, config.ProviderFieldKey(copilotProviderID, "proxy_url"), "socks5://configured:1080"))
	require.Equal(t, "socks5://configured:1080", w.OAuthConfiguredProxy(copilotProviderID))
}

// TestAppWorkspace_OAuthValidateProxy pins that a well-formed value passes
// and a malformed one is rejected, for both providers.
func TestAppWorkspace_OAuthValidateProxy(t *testing.T) {
	w := newOAuthTestWorkspace(t)
	require.NoError(t, w.OAuthValidateProxy(codex.ProviderID, "socks5://127.0.0.1:1080"))
	require.Error(t, w.OAuthValidateProxy(codex.ProviderID, "ftp://nope:21"))
	require.NoError(t, w.OAuthValidateProxy(copilotProviderID, ""))
}

// TestAppWorkspace_CompleteOAuthCodex_RecordsAccountAndProxy_ModelFetchFails
// exercises CompleteOAuth's non-network-dependent side effects (the
// account is recorded, the proxy field is persisted) against a
// deliberately unreachable proxy, so the model fetch that follows fails
// fast (connection refused) instead of reaching the real Codex endpoint or
// hanging: codex.FetchModels' target URL is unexported and not
// swappable from outside the package (see models.go's apiBaseURL var
// comment), so the success path isn't reachable from this layer — see
// login_codex_test.go, whose codexTokenForLogin/fetchCodexModels package
// vars already cover a successful fetch at the CLI layer.
func TestAppWorkspace_CompleteOAuthCodex_RecordsAccountAndProxy_ModelFetchFails(t *testing.T) {
	w := newOAuthTestWorkspace(t)

	token := &oauth.Token{AccessToken: fakeCodexJWT(t, "acct-complete-1", 10*24*time.Hour)}
	// Port 1 is never listening; routing the model fetch's HTTPS request
	// through it as an http proxy fails on the CONNECT immediately rather
	// than timing out.
	const deadProxy = "http://127.0.0.1:1"

	comp, err := w.CompleteOAuth(t.Context(), codex.ProviderID, deadProxy, token, false)
	require.NoError(t, err, "a model-fetch failure must not fail CompleteOAuth itself")
	require.Error(t, comp.ModelsError)
	require.Equal(t, 0, comp.ModelsFetched)
	require.NotEmpty(t, comp.Account.ID)

	accts, err := w.ListAccounts(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, accts, 1)

	require.Equal(t, deadProxy, persistedCodexProxy(t),
		"the proxy used for sign-in must be persisted regardless of the model fetch outcome")
}

// persistedCodexProxy reads providers.codex.proxy_url back out of the
// global config file rather than the store's in-memory snapshot: with no
// models configured for the freshly recorded account, the reload every
// config write triggers fails to pick a default model and leaves the
// in-memory snapshot behind (see ConfigStore.SetConfigFields' "Config file
// updated but failed to reload in-memory state" warning). The file is what
// the write is actually about.
func persistedCodexProxy(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(config.GlobalConfigData())
	require.NoError(t, err)
	var parsed struct {
		Providers map[string]struct {
			ProxyURL string `json:"proxy_url"`
		} `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(data, &parsed))
	return parsed.Providers[codex.ProviderID].ProxyURL
}

// establishExistingCodexLogin runs a real CompleteOAuth so accountID has
// credentials and proxy on file before the test's own call — reading
// providers.codex.proxy_url back out through the config store (as
// completeCodexOAuth's previousProxyURL does) only sees a provider that
// has credentials: the loader drops a credential-less entry from
// cfg.Providers entirely (see internal/providerload/loader.go's
// dropProvider, "missing api_key"), so priming the proxy field alone with
// SetConfigField — without ever recording an account — would make
// previousProxyURL misread as "" regardless of what is actually on disk.
// A real prior sign-in is what these tests are meant to model anyway: the
// guard exists for a re-login whose provider entry already carries a
// possibly-templated proxy_url, not a proxy configured before any login
// ever happened.
func establishExistingCodexLogin(t *testing.T, w *AppWorkspace, accountID, proxyURL string) *oauth.Token {
	t.Helper()
	token := &oauth.Token{AccessToken: fakeCodexJWT(t, accountID, 10*24*time.Hour)}
	_, err := w.CompleteOAuth(t.Context(), codex.ProviderID, proxyURL, token, false)
	require.NoError(t, err)
	require.Equal(t, proxyURL, persistedCodexProxy(t))
	return token
}

// TestAppWorkspace_CompleteOAuthCodex_EmptyProxyRemovesField covers the
// other half of the proxy write: an empty proxyURL must clear whatever the
// provider had configured, not merely skip writing.
func TestAppWorkspace_CompleteOAuthCodex_EmptyProxyRemovesField(t *testing.T) {
	w := newOAuthTestWorkspace(t)
	token := establishExistingCodexLogin(t, w, "acct-complete-2", "socks5://old:1080")

	_, err := w.CompleteOAuth(t.Context(), codex.ProviderID, "", token, false)
	require.NoError(t, err)

	require.Empty(t, persistedCodexProxy(t),
		"an empty proxy must clear the field, not leave the previous value behind")
}

// TestAppWorkspace_CompleteOAuthCodex_SkipsWriteWhenProxyUnchanged pins the
// guard ported from the old CLI's loginCodex/restoreCodexProxyField: a
// sign-in whose proxy already matches what is configured must not touch
// the field at all, not merely rewrite it with the same value — the
// distinction matters when the configured value is an unresolved "$VAR"
// template and proxyURL is what it happened to resolve to. Proven by
// swapping setCodexProxyConfigField/removeCodexProxyConfigField for a pair
// that fail the test if called at all; the priming CompleteOAuth call and
// RecordAccount inside the second use the real implementation.
func TestAppWorkspace_CompleteOAuthCodex_SkipsWriteWhenProxyUnchanged(t *testing.T) {
	w := newOAuthTestWorkspace(t)
	const proxy = "socks5://unchanged:1080"
	token := establishExistingCodexLogin(t, w, "acct-unchanged", proxy)

	origSet, origRemove := setCodexProxyConfigField, removeCodexProxyConfigField
	t.Cleanup(func() { setCodexProxyConfigField, removeCodexProxyConfigField = origSet, origRemove })
	setCodexProxyConfigField = func(*AppWorkspace, config.Scope, string, any) error {
		t.Fatal("an unchanged proxy must never attempt a write")
		return nil
	}
	removeCodexProxyConfigField = func(*AppWorkspace, config.Scope, string) error {
		t.Fatal("an unchanged proxy must never attempt a remove")
		return nil
	}

	comp, err := w.CompleteOAuth(t.Context(), codex.ProviderID, proxy, token, false)
	require.NoError(t, err)
	require.NoError(t, comp.ProxyError, "an unchanged proxy must never attempt a write, so there is nothing to fail")
	require.Equal(t, proxy, persistedCodexProxy(t), "the original value must survive untouched")
}

// TestAppWorkspace_CompleteOAuthCodex_ProxyWriteFailureIsNonFatal covers
// the other half: a proxy that DID change but could not be persisted must
// not fail the sign-in — the credential is already the thing that
// matters — and must not stop the model fetch that follows from running
// with the proxy that was actually used. removeCodexProxyConfigField is
// swapped to fail deterministically rather than relying on a real disk
// failure, which would also break the priming CompleteOAuth call and the
// RecordAccount inside the one under test (both would otherwise go through
// the same config file).
func TestAppWorkspace_CompleteOAuthCodex_ProxyWriteFailureIsNonFatal(t *testing.T) {
	w := newOAuthTestWorkspace(t)
	token := establishExistingCodexLogin(t, w, "acct-proxyfail", "socks5://before:1080")

	origRemove := removeCodexProxyConfigField
	t.Cleanup(func() { removeCodexProxyConfigField = origRemove })
	removeCodexProxyConfigField = func(*AppWorkspace, config.Scope, string) error {
		return errors.New("simulated proxy write failure")
	}

	// proxyURL is "" — different from the primed "socks5://before:1080" --
	// so the guard does not skip the write, and removeCodexProxyConfigField
	// above is what fails it. Port 1 is never listening, so the model
	// fetch that follows the failed proxy write also fails fast rather
	// than reaching a real endpoint.
	comp, err := w.CompleteOAuth(t.Context(), codex.ProviderID, "", token, false)
	require.NoError(t, err, "a proxy write failure must not fail CompleteOAuth itself")
	require.Error(t, comp.ProxyError)
	require.NotEmpty(t, comp.Account.ID, "the account must still be recorded")
	// The model fetch still ran (not skipped because the proxy write
	// failed), and fails on its own terms — there is no real Codex
	// endpoint to reach from a test.
	require.Error(t, comp.ModelsError)
	require.Equal(t, "socks5://before:1080", persistedCodexProxy(t),
		"a failed write must leave the previous value in place, not blank it out")

	accts, err := w.ListAccounts(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, accts, 1, "an account with an unsaved proxy is not an account that failed to save")
}

// TestAppWorkspace_CompleteOAuthCopilot_RecordsAccountWithNoIdentity pins
// Copilot's simpler CompleteOAuth: no account id/email derivation, no
// model fetch (ModelsFetched == -1), matching today's dialog which
// implements none of oauthAccountIDer/oauthAccountEmailer/oauthPostSaver.
func TestAppWorkspace_CompleteOAuthCopilot_RecordsAccountWithNoIdentity(t *testing.T) {
	w := newOAuthTestWorkspace(t)

	token := &oauth.Token{AccessToken: "gho_opaque"}
	comp, err := w.CompleteOAuth(t.Context(), copilotProviderID, "", token, false)
	require.NoError(t, err)
	require.Equal(t, -1, comp.ModelsFetched)
	require.NoError(t, comp.ModelsError)
	require.NotEmpty(t, comp.Account.ID)

	accts, err := w.ListAccounts(copilotProviderID)
	require.NoError(t, err)
	require.Len(t, accts, 1)
}

// TestAppWorkspace_OAuth_UnsupportedProvider is the safety net: no caller
// today asks for a third provider, but both entry points must still fail
// cleanly instead of dispatching nowhere.
func TestAppWorkspace_OAuth_UnsupportedProvider(t *testing.T) {
	w := newOAuthTestWorkspace(t)

	_, _, err := w.StartOAuth(t.Context(), "anthropic", "")
	require.Error(t, err)

	_, err = w.CompleteOAuth(t.Context(), "anthropic", "", &oauth.Token{}, false)
	require.Error(t, err)
}
