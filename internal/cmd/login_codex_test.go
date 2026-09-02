package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	providerstate "github.com/rave-soft/sennit/internal/providers/state"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// codexProviderConfigAccessor extends stubConfigAccessor with a Config()
// that reports a single Codex provider entry, for configuredCodexProxy's
// tests.
type codexProviderConfigAccessor struct {
	stubConfigAccessor
	provider config.ProviderConfig
}

func (s *codexProviderConfigAccessor) Config() *config.Config {
	return &config.Config{
		Providers: csync.NewMap(map[string]config.ProviderConfig{
			codex.ProviderID: s.provider,
		}),
	}
}

// TestConfiguredCodexProxy_UsesConfiguredNotEffective guards the fix for a
// bug where loginCodex's no-flag proxy fallback read ProxyURL — the
// *effective* proxy, resolved for whichever account happened to be active
// — instead of ConfiguredProxyURL, the provider-level default. Reading the
// effective value would promote one account's proxy (or "none", forcing a
// direct connection) to every account's default on the next `sennit login
// codex` with no --proxy flag.
func TestConfiguredCodexProxy_UsesConfiguredNotEffective(t *testing.T) {
	t.Parallel()

	ws := &codexProviderConfigAccessor{provider: config.ProviderConfig{
		ProxyURL: "socks5://configured-proxy:1080",
	}}

	require.Equal(t, "socks5://configured-proxy:1080", configuredCodexProxy(ws))
}

// TestConfiguredCodexProxy_NoProviderYet covers a first-ever login, where
// the Codex provider entry does not exist yet.
func TestConfiguredCodexProxy_NoProviderYet(t *testing.T) {
	t.Parallel()

	ws := &stubConfigAccessor{}
	require.Empty(t, configuredCodexProxy(ws))
}

// codexLoginWorkspaceFake observes the OAuth boundary loginCodex now uses:
// the sign-in itself (StartOAuth/CompleteOAuth) lives behind the
// workspace, so what this fake records is what the CLI asked the backend
// to do, not the individual config/account writes the backend performs on
// its own (those are covered in internal/workspace/appws).
type codexLoginWorkspaceFake struct {
	stubConfigAccessor

	// startResult is what StartOAuth reports; startFlow, when set, is
	// handed back alongside it as the flow to wait on.
	startResult workspace.OAuthStartResult
	startFlow   *stubOAuthFlow
	startErr    error

	// completion/completeErr are CompleteOAuth's answer.
	completion  workspace.OAuthCompletion
	completeErr error

	// configuredProxy is what OAuthConfiguredProxy reports (the Codex
	// CLI's own config, in loginCodex's usage).
	configuredProxy string

	listResults []codexLoginListResult

	calls        []string
	startProxies []string
	completed    []codexCompleteCall
}

type codexCompleteCall struct {
	providerID      string
	proxyURL        string
	token           *oauth.Token
	forceNewAccount bool
}

type codexLoginListResult struct {
	accounts []accounts.Account
	err      error
}

// stubOAuthFlow stands in for a started browser flow: Wait answers with a
// canned token, and Cancel records that the caller released it.
type stubOAuthFlow struct {
	token     *oauth.Token
	err       error
	cancelled int
}

func (f *stubOAuthFlow) Wait(context.Context) (*oauth.Token, error) { return f.token, f.err }
func (f *stubOAuthFlow) Cancel()                                    { f.cancelled++ }

func (w *codexLoginWorkspaceFake) StartOAuth(_ context.Context, providerID, proxyURL string) (workspace.OAuthStartResult, workspace.OAuthFlow, error) {
	w.calls = append(w.calls, "StartOAuth:"+providerID)
	w.startProxies = append(w.startProxies, proxyURL)
	if w.startErr != nil {
		return workspace.OAuthStartResult{}, nil, w.startErr
	}
	if w.startFlow == nil {
		return w.startResult, nil, nil
	}
	return w.startResult, w.startFlow, nil
}

func (w *codexLoginWorkspaceFake) CompleteOAuth(_ context.Context, providerID, proxyURL string, token *oauth.Token, forceNewAccount bool) (workspace.OAuthCompletion, error) {
	w.calls = append(w.calls, "CompleteOAuth:"+providerID)
	w.completed = append(w.completed, codexCompleteCall{providerID, proxyURL, token, forceNewAccount})
	return w.completion, w.completeErr
}

func (w *codexLoginWorkspaceFake) OAuthConfiguredProxy(string) string { return w.configuredProxy }

func (w *codexLoginWorkspaceFake) OAuthValidateProxy(_, proxyURL string) error {
	if proxyURL == "bad-proxy" {
		return errors.New("invalid proxy")
	}
	return nil
}

func (w *codexLoginWorkspaceFake) ListAccounts(providerID string) ([]accounts.Account, error) {
	w.calls = append(w.calls, "ListAccounts:"+providerID)
	result := w.listResults[0]
	w.listResults = w.listResults[1:]
	return result.accounts, result.err
}

// newCodexLoginFake builds a fake whose sign-in short-circuits on an
// existing Codex CLI login, so no test here needs the interactive
// browser step (which would block on stdin).
func newCodexLoginFake(before, after []accounts.Account) *codexLoginWorkspaceFake {
	return &codexLoginWorkspaceFake{
		startResult: workspace.OAuthStartResult{
			Token:               &oauth.Token{AccessToken: "access-token"},
			ReusedExistingLogin: true,
		},
		completion: workspace.OAuthCompletion{
			Account:       accounts.Account{ID: "new", Label: "New account"},
			ModelsFetched: 1,
		},
		listResults: []codexLoginListResult{{accounts: before}, {accounts: after}},
	}
}

// TestLoginCodex_StartsThenCompletesSignIn pins the boundary: the CLI asks
// the workspace to start the flow and to finish it, counting accounts
// around the completion for its summary line, and never performs the
// account/config writes itself.
func TestLoginCodex_StartsThenCompletesSignIn(t *testing.T) {
	t.Parallel()

	ws := newCodexLoginFake(
		[]accounts.Account{{ID: "existing"}},
		[]accounts.Account{{ID: "existing"}, {ID: "new"}},
	)

	require.NoError(t, loginCodex(ws, true, ""))
	require.Equal(t, []string{
		"StartOAuth:codex", "ListAccounts:codex", "CompleteOAuth:codex", "ListAccounts:codex",
	}, ws.calls)
	require.Len(t, ws.completed, 1)
	require.Equal(t, "access-token", ws.completed[0].token.AccessToken)
	require.False(t, ws.completed[0].forceNewAccount)
}

// TestLoginCodex_FirstAccountListingFailureDoesNotComplete keeps the
// pre-existing ordering guarantee: a failure to count accounts happens
// before anything is persisted, so nothing is recorded.
func TestLoginCodex_FirstAccountListingFailureDoesNotComplete(t *testing.T) {
	t.Parallel()

	listErr := errors.New("account store unavailable")
	ws := newCodexLoginFake(nil, nil)
	ws.listResults = []codexLoginListResult{{err: listErr}}

	err := loginCodex(ws, true, "")
	require.ErrorIs(t, err, listErr)
	require.Equal(t, []string{"StartOAuth:codex", "ListAccounts:codex"}, ws.calls)
	require.Empty(t, ws.completed)
}

// TestLoginCodex_SecondAccountListingFailureKeepsSuccessfulLogin: the
// sign-in already succeeded by the time the summary re-list runs, so a
// failure there must not fail the command.
func TestLoginCodex_SecondAccountListingFailureKeepsSuccessfulLogin(t *testing.T) {
	t.Parallel()

	ws := newCodexLoginFake(nil, nil)
	ws.listResults = []codexLoginListResult{{}, {err: errors.New("account store unavailable")}}

	require.NoError(t, loginCodex(ws, true, ""))
	require.Len(t, ws.completed, 1, "the account is persisted before the summary re-list")
}

// TestLoginCodex_ModelFetchFailureIsNotFatal pins the non-fatal treatment
// of a model-list failure: the credential is already saved by the time
// CompleteOAuth reports it, so the command reports the problem and
// succeeds.
func TestLoginCodex_ModelFetchFailureIsNotFatal(t *testing.T) {
	t.Parallel()

	ws := newCodexLoginFake(nil, nil)
	ws.completion = workspace.OAuthCompletion{
		Account:     accounts.Account{ID: "new", Label: "New account"},
		ModelsError: errors.New("model list unavailable"),
	}

	require.NoError(t, loginCodex(ws, true, ""))
	require.Len(t, ws.completed, 1)
}

// TestLoginCodex_CompleteFailureIsFatal covers the other half: a
// credential that could not be recorded at all is a failed login.
func TestLoginCodex_CompleteFailureIsFatal(t *testing.T) {
	t.Parallel()

	completeErr := errors.New("account store unavailable")
	ws := newCodexLoginFake(nil, nil)
	ws.completeErr = completeErr

	require.ErrorIs(t, loginCodex(ws, true, ""), completeErr)
}

// TestLoginCodex_ProxyWriteFailureIsFatal pins the other non-fatal field's
// opposite treatment: unlike ModelsError, a proxy that could not be
// persisted fails the command outright, matching what a failed proxy
// write did before this refactor (it aborted the login before the
// account was even recorded — the credential here just happens to
// already be saved, which is an acceptable, documented change in
// end-state on this rare failure path, not a visible prompt/message
// change).
func TestLoginCodex_ProxyWriteFailureIsFatal(t *testing.T) {
	t.Parallel()

	proxyErr := errors.New("signed in, but the proxy setting could not be saved: disk full")
	ws := newCodexLoginFake(nil, nil)
	ws.completion = workspace.OAuthCompletion{
		Account:    accounts.Account{ID: "new", Label: "New account"},
		ProxyError: proxyErr,
	}

	require.ErrorIs(t, loginCodex(ws, true, ""), proxyErr)
}

// TestLoginCodex_ProxyResolutionOrder pins flag > configured > the Codex
// CLI's own on-disk proxy, which is what the workspace's
// OAuthConfiguredProxy answers once nothing is configured here.
func TestLoginCodex_ProxyResolutionOrder(t *testing.T) {
	t.Parallel()

	t.Run("flag wins", func(t *testing.T) {
		t.Parallel()
		ws := newCodexLoginFake(nil, nil)
		ws.configuredProxy = "socks5://from-cli:1080"
		require.NoError(t, loginCodex(ws, true, "http://flag:8080"))
		require.Equal(t, []string{"http://flag:8080"}, ws.startProxies)
		require.Equal(t, "http://flag:8080", ws.completed[0].proxyURL)
	})

	t.Run("configured provider proxy is next", func(t *testing.T) {
		t.Parallel()
		ws := &codexLoginWorkspaceFake{
			startResult: workspace.OAuthStartResult{
				Token: &oauth.Token{AccessToken: "access-token"}, ReusedExistingLogin: true,
			},
			listResults:     []codexLoginListResult{{}, {}},
			configuredProxy: "socks5://from-cli:1080",
		}
		ws.stubConfigAccessor = stubConfigAccessor{}
		ws.completion = workspace.OAuthCompletion{Account: accounts.Account{Label: "acct"}}
		require.NoError(t, loginCodexWithConfiguredProxy(t, ws, "socks5://configured:1080"))
		require.Equal(t, []string{"socks5://configured:1080"}, ws.startProxies)
	})

	t.Run("codex CLI proxy is the last resort", func(t *testing.T) {
		t.Parallel()
		ws := newCodexLoginFake(nil, nil)
		ws.configuredProxy = "socks5://from-cli:1080"
		require.NoError(t, loginCodex(ws, true, ""))
		require.Equal(t, []string{"socks5://from-cli:1080"}, ws.startProxies)
	})
}

// codexLoginConfiguredProxyFake reports a configured provider proxy, for
// the middle rung of TestLoginCodex_ProxyResolutionOrder.
type codexLoginConfiguredProxyFake struct {
	*codexLoginWorkspaceFake
	proxyURL string
}

func (w *codexLoginConfiguredProxyFake) Config() *config.Config {
	return &config.Config{
		Providers: csync.NewMap(map[string]config.ProviderConfig{
			codex.ProviderID: {ID: codex.ProviderID, ProxyURL: w.proxyURL},
		}),
	}
}

func loginCodexWithConfiguredProxy(t *testing.T, ws *codexLoginWorkspaceFake, proxyURL string) error {
	t.Helper()
	return loginCodex(&codexLoginConfiguredProxyFake{codexLoginWorkspaceFake: ws, proxyURL: proxyURL}, true, "")
}

// TestLoginCodex_AlreadyLoggedInShortCircuits keeps the --force contract:
// without it, an existing token means nothing is started at all.
func TestLoginCodex_AlreadyLoggedInShortCircuits(t *testing.T) {
	t.Parallel()

	inner := newCodexLoginFake(nil, nil)
	ws := &codexLoginTokenFake{codexLoginWorkspaceFake: inner}

	require.NoError(t, loginCodex(ws, false, ""))
	require.Empty(t, inner.calls, "an existing login must not start a new sign-in")
}

// codexLoginTokenFake reports a Codex provider that already holds an OAuth
// token, for the "already logged in" short-circuit.
type codexLoginTokenFake struct {
	*codexLoginWorkspaceFake
}

func (w *codexLoginTokenFake) Config() *config.Config {
	return &config.Config{
		Providers: csync.NewMap(map[string]config.ProviderConfig{
			codex.ProviderID: {ID: codex.ProviderID},
		}),
		RuntimeProviders: csync.NewMap(map[string]providerstate.Provider{
			codex.ProviderID: {ID: codex.ProviderID, OAuthToken: &oauth.Token{AccessToken: "existing"}},
		}),
	}
}

// TestLoginCodex_RejectsBadProxy: validation happens before anything goes
// out on the network, through the workspace's own validator.
func TestLoginCodex_RejectsBadProxy(t *testing.T) {
	t.Parallel()

	ws := newCodexLoginFake(nil, nil)
	require.Error(t, loginCodex(ws, true, "bad-proxy"))
	require.Empty(t, ws.calls)
}
