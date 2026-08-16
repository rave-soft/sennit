package codex

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeJWT builds an unsigned token whose payload carries the given account
// claim. Nothing here verifies signatures — the account ID is read out of a
// token the authorization server already issued to us — so an unsigned one
// exercises the same path.
func fakeJWT(t *testing.T, accountID string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
	})
	require.NoError(t, err)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

func TestAccountIDReadsClaim(t *testing.T) {
	t.Parallel()

	require.Equal(t, "acct-123", AccountID(fakeJWT(t, "acct-123")))
}

// TestAccountIDToleratesNonJWT pins the "let the backend decide" contract:
// an opaque or malformed token yields "", not a panic and not a bogus ID.
func TestAccountIDToleratesNonJWT(t *testing.T) {
	t.Parallel()

	for _, token := range []string{"", "opaque-token", "a.b", "a.!!!.c", fakeJWT(t, "")} {
		require.Empty(t, AccountID(token))
	}
}

// TestHeadersOmitAccountWhenUnknown: an empty account must not become an
// empty header, which the backend would read as an explicit "no account".
func TestHeadersOmitAccountWhenUnknown(t *testing.T) {
	t.Parallel()

	headers := Headers("")
	require.NotContains(t, headers, "chatgpt-account-id")
	require.Equal(t, "codex_cli_rs", headers["originator"])

	headers = Headers("acct-1")
	require.Equal(t, "acct-1", headers["chatgpt-account-id"])
}

func TestTokensFromDisk(t *testing.T) {
	// No t.Parallel: t.Setenv pins CODEX_HOME for this test.
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	// No file at all: nothing to import.
	_, ok := TokensFromDisk()
	require.False(t, ok)

	auth := map[string]any{
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token":      fakeJWT(t, "acct-from-id-token"),
			"access_token":  "at",
			"refresh_token": "rt",
		},
	}
	data, err := json.Marshal(auth)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, "auth.json"), data, 0o600))

	tokens, ok := TokensFromDisk()
	require.True(t, ok)
	require.Equal(t, "rt", tokens.RefreshToken)
	require.Equal(t, "at", tokens.AccessToken)
	require.Equal(t, "acct-from-id-token", tokens.AccountID,
		"the account must be recovered from the ID token when the file has no account_id")
}

// TestTokensFromDiskNeedsRefreshToken: a login file without a refresh token
// is useless to us — the access token in it may well be expired, and there
// would be no way to get another.
func TestTokensFromDiskNeedsRefreshToken(t *testing.T) {
	// No t.Parallel: t.Setenv pins CODEX_HOME for this test.
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "auth.json"),
		[]byte(`{"tokens":{"access_token":"at"}}`), 0o600))

	_, ok := TokensFromDisk()
	require.False(t, ok)
}

// TestStartFlowBuildsPKCEURL checks the parts of the authorization URL the
// server actually enforces: the fixed redirect URI, S256 PKCE, and a state
// to bind the callback to this request.
func TestStartFlowBuildsPKCEURL(t *testing.T) {
	flow, err := StartFlow("")
	require.NoError(t, err)
	t.Cleanup(func() { _ = flow.Close() })

	parsed, err := url.Parse(flow.URL())
	require.NoError(t, err)
	query := parsed.Query()

	require.Equal(t, "https://auth.openai.com/oauth/authorize", parsed.Scheme+"://"+parsed.Host+parsed.Path)
	require.Equal(t, "code", query.Get("response_type"))
	require.Equal(t, "http://localhost:1455/auth/callback", query.Get("redirect_uri"))
	require.Equal(t, "S256", query.Get("code_challenge_method"))
	require.NotEmpty(t, query.Get("code_challenge"))
	require.NotEmpty(t, query.Get("state"))
	require.Contains(t, query.Get("scope"), "offline_access")
}

// TestStartFlowPortIsExclusive: the redirect URI names one fixed port, so a
// second concurrent sign-in has to fail loudly rather than bind elsewhere
// and wait for a redirect that will never come.
func TestStartFlowPortIsExclusive(t *testing.T) {
	first, err := StartFlow("")
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Close() })

	_, err = StartFlow("")
	require.Error(t, err)
}

// TestFlowRejectsMismatchedState: a callback that does not carry this
// flow's state belongs to some other authorization and must not settle it.
func TestFlowRejectsMismatchedState(t *testing.T) {
	flow, err := StartFlow("")
	require.NoError(t, err)
	t.Cleanup(func() { _ = flow.Close() })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, callbackPath+"?code=abc&state=not-the-state", nil)
	flow.handleCallback(rec, req)

	res := <-flow.results
	require.Error(t, res.err)
	require.Empty(t, res.code)
}

// TestFlowIgnoresUnrelatedPaths: browsers fetch /favicon.ico against the
// same origin, and settling on one of those would abort the sign-in with an
// empty code.
func TestFlowIgnoresUnrelatedPaths(t *testing.T) {
	flow, err := StartFlow("")
	require.NoError(t, err)
	t.Cleanup(func() { _ = flow.Close() })

	rec := httptest.NewRecorder()
	flow.handleCallback(rec, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))

	require.Equal(t, http.StatusNotFound, rec.Code)
	select {
	case res := <-flow.results:
		t.Fatalf("an unrelated request settled the flow: %+v", res)
	default:
	}
}

// TestProxyFromDisk reads the proxy out of the Codex CLI's own config, so a
// user who already told the CLI about their proxy is not asked again.
func TestProxyFromDisk(t *testing.T) {
	// No t.Parallel: t.Setenv pins CODEX_HOME for this test.
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	require.Empty(t, ProxyFromDisk(), "no config file means nothing to borrow")

	write := func(body string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600))
	}

	write(`
# a comment mentioning proxy_url = "http://commented-out:1"
model = "gpt-5.6-sol"

[network]
proxy_url = "http://127.0.0.1:8080"
`)
	require.Equal(t, "http://127.0.0.1:8080", ProxyFromDisk())

	// The CLI accepts either table name, so both are read.
	write("[network_proxy]\nproxy_url = 'http://other:3128'\n")
	require.Equal(t, "http://other:3128", ProxyFromDisk())

	// A SOCKS-only setup names the address under socks_url instead.
	write("[network]\nenable_socks5 = true\nsocks_url = \"socks5://127.0.0.1:1080\"\n")
	require.Equal(t, "socks5://127.0.0.1:1080", ProxyFromDisk())

	// An explicit proxy_url wins over the socks fallback.
	write("[network]\nsocks_url = \"socks5://127.0.0.1:1080\"\nproxy_url = \"http://127.0.0.1:8080\"\n")
	require.Equal(t, "http://127.0.0.1:8080", ProxyFromDisk())
}

// TestProxyFromDiskIgnoresOtherTables: proxy_url under some unrelated
// section belongs to that section, not to the network settings.
func TestProxyFromDiskIgnoresOtherTables(t *testing.T) {
	// No t.Parallel: t.Setenv pins CODEX_HOME for this test.
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[mcp_servers.something]\nproxy_url = \"http://not-ours:1\"\n"), 0o600))

	require.Empty(t, ProxyFromDisk())
}

// TestTokensFromDiskCarriesProxy: the proxy rides along with the login, so
// importing credentials brings the setting the user already made.
func TestTokensFromDiskCarriesProxy(t *testing.T) {
	// No t.Parallel: t.Setenv pins CODEX_HOME for this test.
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "auth.json"),
		[]byte(`{"tokens":{"access_token":"at","refresh_token":"rt"}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[network]\nproxy_url = \"http://127.0.0.1:8080\"\n"), 0o600))

	tokens, ok := TokensFromDisk()
	require.True(t, ok)
	require.Equal(t, "http://127.0.0.1:8080", tokens.ProxyURL)
}
