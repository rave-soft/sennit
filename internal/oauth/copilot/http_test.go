package copilot

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestHTTPClientRoutesThroughProxy proves a non-empty proxy URL produces a
// client whose transport is proxy-configured — the whole point for a user
// who can only reach GitHub through one.
func TestHTTPClientRoutesThroughProxy(t *testing.T) {
	t.Parallel()

	client, err := httpClient("http://127.0.0.1:8080", 5*time.Second)
	require.NoError(t, err)

	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, transport.Proxy, "a configured proxy must set the transport's Proxy func")

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", nil)
	require.NoError(t, err)
	proxyURL, err := transport.Proxy(req)
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:8080", proxyURL.String())
	require.Equal(t, 5*time.Second, client.Timeout)
}

// TestHTTPClientEmptyLeavesDefaultTransport pins that an empty proxy URL
// behaves exactly as before this package learned about proxies: a plain
// client with no transport override.
func TestHTTPClientEmptyLeavesDefaultTransport(t *testing.T) {
	t.Parallel()

	client, err := httpClient("", 30*time.Second)
	require.NoError(t, err)
	require.Nil(t, client.Transport, "no proxy override must leave the default transport alone")
	require.Equal(t, 30*time.Second, client.Timeout)
}
