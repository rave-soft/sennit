package codex

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestValidateProxy(t *testing.T) {
	t.Parallel()

	for _, proxy := range []string{
		"",
		ProxyDirect,
		"http://127.0.0.1:8080",
		"https://proxy.example:3128",
		"socks5://127.0.0.1:1080",
		"socks5h://127.0.0.1:1080",
	} {
		require.NoError(t, ValidateProxy(proxy), "proxy %q must be accepted", proxy)
	}

	for _, proxy := range []string{
		"ftp://127.0.0.1:21",
		"127.0.0.1:1080", // no scheme: parsed as a path, not a proxy
		"://nope",
	} {
		require.Error(t, ValidateProxy(proxy), "proxy %q must be rejected", proxy)
	}
}

// TestHTTPClientRoutesThroughProxy proves the client actually goes through
// the proxy rather than merely accepting the setting — the whole point for a
// user who cannot reach OpenAI directly.
func TestHTTPClientRoutesThroughProxy(t *testing.T) {
	t.Parallel()

	var proxied []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = append(proxied, r.Host)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(proxy.Close)

	client, err := httpClient(proxy.URL, 5*time.Second)
	require.NoError(t, err)

	// An http:// target is forwarded through the proxy as a plain request,
	// so the test server sees it without having to speak CONNECT.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://codex.invalid/models", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{"codex.invalid"}, proxied)
}

// TestHTTPClientDirectIgnoresEnv: "none" must beat HTTP_PROXY, which is the
// only way to force a direct connection from a machine whose environment
// points at a proxy.
func TestHTTPClientDirectIgnoresEnv(t *testing.T) {
	// No t.Parallel: t.Setenv pins the proxy environment.
	t.Setenv("HTTP_PROXY", "http://should-not-be-used.invalid:9")

	client, err := httpClient(ProxyDirect, time.Second)
	require.NoError(t, err)

	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.Nil(t, transport.Proxy, "a direct client must not consult the environment")

	// An empty proxy leaves the default behaviour in place instead.
	client, err = httpClient("", time.Second)
	require.NoError(t, err)
	require.Nil(t, client.Transport, "no proxy override must leave the default transport alone")
}

// TestStartFlowRejectsBadProxy: a bad proxy must fail before the callback
// port is bound, otherwise a doomed sign-in would hold the one port the
// redirect URI names.
func TestStartFlowRejectsBadProxy(t *testing.T) {
	flow, err := StartFlow("ftp://nope:21")
	require.Error(t, err)
	require.Nil(t, flow)

	// The port is still free, proving nothing was left bound.
	require.NotNil(t, startTestFlow(t, ""))
}

// TestFlowKeepsProxyForExchange pins that the value reaches the token
// exchange: the redirect arrives at a loopback listener that needs no proxy,
// but the exchange that follows it does.
func TestFlowKeepsProxyForExchange(t *testing.T) {
	flow := startTestFlow(t, "socks5://127.0.0.1:1080")

	require.Equal(t, "socks5://127.0.0.1:1080", flow.proxyURL)

	// The authorization URL is unaffected by the proxy; it is opened by the
	// user's browser, which has proxy settings of its own.
	parsed, err := url.Parse(flow.URL())
	require.NoError(t, err)
	require.Equal(t, "auth.openai.com", parsed.Host)
}
