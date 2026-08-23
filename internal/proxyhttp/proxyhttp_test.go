package proxyhttp

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestNewClientDirectIgnoresTheEnvironment pins the sentinel: "none" means
// connect straight out even when HTTP_PROXY names a proxy, which is the
// whole reason it exists.
func TestNewClientDirectIgnoresTheEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")

	client, err := NewClient(Direct, time.Second)
	require.NoError(t, err)

	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.Nil(t, transport.Proxy, "a direct client must not consult the environment")
	require.Equal(t, time.Second, client.Timeout)
}

// TestNewClientRoutesThroughTheProxy covers the ordinary case.
func TestNewClientRoutesThroughTheProxy(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"http://proxy:8080", "https://proxy:8443", "socks5://proxy:1080", "socks5h://proxy:1080"} {
		client, err := NewClient(raw, 0)
		require.NoError(t, err, raw)

		transport, ok := client.Transport.(*http.Transport)
		require.True(t, ok)
		require.NotNil(t, transport.Proxy)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", nil)
		require.NoError(t, err)
		proxyURL, err := transport.Proxy(req)
		require.NoError(t, err)
		require.Equal(t, raw, proxyURL.String())
	}
}

// TestNewClientRejectsWhatItCannotRoute keeps the validation the UI relies
// on to reject a bad value while the person is still looking at the field.
func TestNewClientRejectsWhatItCannotRoute(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"ftp://proxy:21", "://nope", "gopher://proxy"} {
		_, err := NewClient(raw, 0)
		require.Error(t, err, raw)
	}
}
