package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	providerstate "github.com/rave-soft/sennit/internal/providers/state"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// codexProviderConfigAccessor stands in for a Workspace, answering only
// OAuthProviderConfiguredProxy, for configuredCodexProxy's own tests.
type codexProviderConfigAccessor struct {
	proxyURL string
}

func (s *codexProviderConfigAccessor) OAuthProviderConfiguredProxy(string) string {
	return s.proxyURL
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

	ws := &codexProviderConfigAccessor{proxyURL: "socks5://configured-proxy:1080"}

	require.Equal(t, "socks5://configured-proxy:1080", configuredCodexProxy(ws))
}

// TestConfiguredCodexProxy_NoProviderYet covers a first-ever login, where
// the Codex provider entry does not exist yet.
func TestConfiguredCodexProxy_NoProviderYet(t *testing.T) {
	t.Parallel()

	ws := &codexProviderConfigAccessor{}
	require.Empty(t, configuredCodexProxy(ws))
}

// codexLoginWorkspaceFake observes the OAuth boundary loginCodex now uses:
// the sign-in itself, including completing it, lives behind the workspace
// (StartOAuth/OAuthFlow.Wait — see workspace.OAuthController and
// CLIENT-SERVER.md PR 1.3), so what this fake records is what the CLI
// asked the backend to do, not the individual config/account writes the
// backend performs on its own (those are covered in
// internal/workspace/appws).
type codexLoginWorkspaceFake struct {
	stubConfigAccessor

	// startResult is what StartOAuth reports; startFlow, when set, is
	// handed back alongside it as the flow to wait on.
	startResult workspace.OAuthStartResult
	startFlow   *stubOAuthFlow
	startErr    error

	// configuredProxy is what OAuthConfiguredProxy reports (the Codex
	// CLI's own config, in loginCodex's usage).
	configuredProxy string

	listResults []codexLoginListResult

	calls         []string
	startProxies  []string
	startForceNew []bool
}

type codexLoginListResult struct {
	accounts []workspace.FrontendAccount
	err      error
}

// stubOAuthFlow stands in for a started browser flow: Wait answers with a
// canned completion, and Cancel records that the caller released it.
type stubOAuthFlow struct {
	completion workspace.OAuthCompletion
	err        error
	cancelled  int
}

func (f *stubOAuthFlow) Wait(context.Context) (workspace.OAuthCompletion, error) {
	return f.completion, f.err
}

func (f *stubOAuthFlow) Cancel() { f.cancelled++ }

func (w *codexLoginWorkspaceFake) StartOAuth(_ context.Context, providerID, proxyURL string, forceNewAccount bool) (workspace.OAuthStartResult, workspace.OAuthFlow, error) {
	w.calls = append(w.calls, "StartOAuth:"+providerID)
	w.startProxies = append(w.startProxies, proxyURL)
	w.startForceNew = append(w.startForceNew, forceNewAccount)
	if w.startErr != nil {
		return workspace.OAuthStartResult{}, nil, w.startErr
	}
	if w.startFlow == nil {
		return w.startResult, nil, nil
	}
	return w.startResult, w.startFlow, nil
}

func (w *codexLoginWorkspaceFake) OAuthConfiguredProxy(string) string { return w.configuredProxy }

// OAuthProviderConfiguredProxy defaults to "" (nothing configured in
// Sennit's own config yet, see configuredCodexProxy's doc comment);
// codexLoginConfiguredProxyFake overrides it for the "configured provider
// proxy" rung of TestLoginCodex_ProxyResolutionOrder.
func (w *codexLoginWorkspaceFake) OAuthProviderConfiguredProxy(string) string { return "" }

func (w *codexLoginWorkspaceFake) OAuthValidateProxy(_, proxyURL string) error {
	if proxyURL == "bad-proxy" {
		return errors.New("invalid proxy")
	}
	return nil
}

func (w *codexLoginWorkspaceFake) ListAccounts(providerID string) ([]workspace.FrontendAccount, error) {
	w.calls = append(w.calls, "ListAccounts:"+providerID)
	result := w.listResults[0]
	w.listResults = w.listResults[1:]
	return result.accounts, result.err
}

// newCodexLoginFake builds a fake whose sign-in short-circuits on an
// existing Codex CLI login, so no test here needs the interactive
// browser step (which would block on stdin). The server has already
// completed the sign-in by the time StartOAuth returns, matching
// AppWorkspace.startCodexOAuth's disk-reuse path.
func newCodexLoginFake(before, after []workspace.FrontendAccount) *codexLoginWorkspaceFake {
	completion := workspace.OAuthCompletion{
		Account:       workspace.FrontendAccount{ID: "new", Label: "New account"},
		ModelsFetched: 1,
	}
	return &codexLoginWorkspaceFake{
		startResult: workspace.OAuthStartResult{
			Completed:           &completion,
			ReusedExistingLogin: true,
		},
		listResults: []codexLoginListResult{{accounts: before}, {accounts: after}},
	}
}

// TestLoginCodex_StartsSignIn pins the boundary: the CLI asks the
// workspace to start the flow, which completes it server-side, counting
// accounts around the completion for its summary line, and never performs
// the account/config writes itself.
func TestLoginCodex_StartsSignIn(t *testing.T) {
	t.Parallel()

	ws := newCodexLoginFake(
		[]workspace.FrontendAccount{{ID: "existing"}},
		[]workspace.FrontendAccount{{ID: "existing"}, {ID: "new"}},
	)

	require.NoError(t, loginCodex(ws, true, false, "", recordLoginIO(t)))
	require.Equal(t, []string{
		"StartOAuth:codex", "ListAccounts:codex", "ListAccounts:codex",
	}, ws.calls)
	require.Equal(t, []bool{false}, ws.startForceNew)
}

// TestLoginCodex_FirstAccountListingFailureIsFatal keeps the pre-existing
// ordering guarantee: a failure to count accounts happens before anything
// is reported as a success, so the command fails.
func TestLoginCodex_FirstAccountListingFailureIsFatal(t *testing.T) {
	t.Parallel()

	listErr := errors.New("account store unavailable")
	ws := newCodexLoginFake(nil, nil)
	ws.listResults = []codexLoginListResult{{err: listErr}}

	err := loginCodex(ws, true, false, "", recordLoginIO(t))
	require.ErrorIs(t, err, listErr)
	require.Equal(t, []string{"StartOAuth:codex", "ListAccounts:codex"}, ws.calls)
}

// TestLoginCodex_SecondAccountListingFailureKeepsSuccessfulLogin: the
// sign-in already succeeded by the time the summary re-list runs, so a
// failure there must not fail the command.
func TestLoginCodex_SecondAccountListingFailureKeepsSuccessfulLogin(t *testing.T) {
	t.Parallel()

	ws := newCodexLoginFake(nil, nil)
	ws.listResults = []codexLoginListResult{{}, {err: errors.New("account store unavailable")}}

	require.NoError(t, loginCodex(ws, true, false, "", recordLoginIO(t)))
}

// TestLoginCodex_ModelFetchFailureIsNotFatal pins the non-fatal treatment
// of a model-list failure: the credential is already saved by the time the
// completion reports it, so the command reports the problem and succeeds.
func TestLoginCodex_ModelFetchFailureIsNotFatal(t *testing.T) {
	t.Parallel()

	ws := newCodexLoginFake(nil, nil)
	completion := workspace.OAuthCompletion{
		Account:     workspace.FrontendAccount{ID: "new", Label: "New account"},
		ModelsError: workspace.EncodeError(errors.New("model list unavailable")),
	}
	ws.startResult.Completed = &completion

	require.NoError(t, loginCodex(ws, true, false, "", recordLoginIO(t)))
}

// TestLoginCodex_StartOAuthFailureIsFatal covers a credential that could
// not be recorded at all (StartOAuth fails when the server's own
// completion step fails - see AppWorkspace.startCodexOAuth): a failed
// login, and nothing beyond StartOAuth is attempted.
func TestLoginCodex_StartOAuthFailureIsFatal(t *testing.T) {
	t.Parallel()

	startErr := errors.New("account store unavailable")
	ws := newCodexLoginFake(nil, nil)
	ws.startErr = startErr

	require.ErrorIs(t, loginCodex(ws, true, false, "", recordLoginIO(t)), startErr)
	require.Equal(t, []string{"StartOAuth:codex"}, ws.calls)
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
	completion := workspace.OAuthCompletion{
		Account:    workspace.FrontendAccount{ID: "new", Label: "New account"},
		ProxyError: workspace.EncodeError(proxyErr),
	}
	ws.startResult.Completed = &completion

	// proxyErr is an opaque error with no registered wireerr code, so it
	// crosses OAuthCompletion.ProxyError as "internal": DecodeError
	// preserves its text but not its identity (see DecodeError's doc
	// comment) — the same tradeoff every other opaque error takes once it
	// is carried on a DTO field, per this package's wireerr conversion.
	err := loginCodex(ws, true, false, "", recordLoginIO(t))
	require.ErrorContains(t, err, proxyErr.Error())
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
		require.NoError(t, loginCodex(ws, true, false, "http://flag:8080", recordLoginIO(t)))
		require.Equal(t, []string{"http://flag:8080"}, ws.startProxies)
	})

	t.Run("configured provider proxy is next", func(t *testing.T) {
		t.Parallel()
		completion := workspace.OAuthCompletion{Account: workspace.FrontendAccount{Label: "acct"}}
		ws := &codexLoginWorkspaceFake{
			startResult: workspace.OAuthStartResult{
				Completed:           &completion,
				ReusedExistingLogin: true,
			},
			listResults:     []codexLoginListResult{{}, {}},
			configuredProxy: "socks5://from-cli:1080",
		}
		ws.stubConfigAccessor = stubConfigAccessor{}
		require.NoError(t, loginCodexWithConfiguredProxy(t, ws, "socks5://configured:1080"))
		require.Equal(t, []string{"socks5://configured:1080"}, ws.startProxies)
	})

	t.Run("codex CLI proxy is the last resort", func(t *testing.T) {
		t.Parallel()
		ws := newCodexLoginFake(nil, nil)
		ws.configuredProxy = "socks5://from-cli:1080"
		require.NoError(t, loginCodex(ws, true, false, "", recordLoginIO(t)))
		require.Equal(t, []string{"socks5://from-cli:1080"}, ws.startProxies)
	})
}

// codexLoginConfiguredProxyFake reports a configured provider proxy, for
// the middle rung of TestLoginCodex_ProxyResolutionOrder.
type codexLoginConfiguredProxyFake struct {
	*codexLoginWorkspaceFake
	proxyURL string
}

func (w *codexLoginConfiguredProxyFake) OAuthProviderConfiguredProxy(string) string {
	return w.proxyURL
}

func loginCodexWithConfiguredProxy(t *testing.T, ws *codexLoginWorkspaceFake, proxyURL string) error {
	t.Helper()
	return loginCodex(&codexLoginConfiguredProxyFake{codexLoginWorkspaceFake: ws, proxyURL: proxyURL}, true, false, "", recordLoginIO(t))
}

// TestLoginCodex_AlreadyLoggedInShortCircuits keeps the --force contract:
// without it, an existing token means nothing is started at all.
func TestLoginCodex_AlreadyLoggedInShortCircuits(t *testing.T) {
	t.Parallel()

	inner := newCodexLoginFake(nil, nil)
	ws := &codexLoginTokenFake{codexLoginWorkspaceFake: inner}

	require.NoError(t, loginCodex(ws, false, false, "", recordLoginIO(t)))
	require.Empty(t, inner.calls, "an existing login must not start a new sign-in")
}

// codexLoginTokenFake reports a Codex provider that already holds an OAuth
// token, for the "already logged in" short-circuit.
type codexLoginTokenFake struct {
	*codexLoginWorkspaceFake
}

func (w *codexLoginTokenFake) rawConfig() *config.Config {
	return &config.Config{
		Providers: csync.NewMap(map[string]config.ProviderConfig{
			codex.ProviderID: {ID: codex.ProviderID},
		}),
		RuntimeProviders: csync.NewMap(map[string]providerstate.Provider{
			codex.ProviderID: {ID: codex.ProviderID, OAuthToken: &oauth.Token{AccessToken: "existing"}},
		}),
	}
}

func (w *codexLoginTokenFake) Config() *workspace.FrontendConfig {
	return workspace.NewFrontendConfig(w.rawConfig(), nil)
}

// ServerConfig satisfies workspace.ServerConfigReader, which loginCodex
// (serverConfig in server_config.go) type-asserts for.
func (w *codexLoginTokenFake) ServerConfig() *config.Config {
	return w.rawConfig()
}

// TestLoginCodex_RejectsBadProxy: validation happens before anything goes
// out on the network, through the workspace's own validator.
func TestLoginCodex_RejectsBadProxy(t *testing.T) {
	t.Parallel()

	ws := newCodexLoginFake(nil, nil)
	require.Error(t, loginCodex(ws, true, false, "bad-proxy", recordLoginIO(t)))
	require.Empty(t, ws.calls)
}
