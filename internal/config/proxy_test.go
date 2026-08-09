package config

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewProxyHTTPClient(t *testing.T) {
	t.Parallel()

	t.Run("empty proxy URL returns nil client and nil error", func(t *testing.T) {
		t.Parallel()
		client, err := NewProxyHTTPClient("")
		require.NoError(t, err)
		require.Nil(t, client)
	})

	t.Run("http scheme", func(t *testing.T) {
		t.Parallel()
		client, err := NewProxyHTTPClient("http://localhost:8080")
		require.NoError(t, err)
		require.NotNil(t, client)
	})

	t.Run("https scheme", func(t *testing.T) {
		t.Parallel()
		client, err := NewProxyHTTPClient("https://proxy.example.com:8443")
		require.NoError(t, err)
		require.NotNil(t, client)
	})

	t.Run("socks5 scheme", func(t *testing.T) {
		t.Parallel()
		client, err := NewProxyHTTPClient("socks5://localhost:1080")
		require.NoError(t, err)
		require.NotNil(t, client)
	})

	t.Run("invalid URL syntax returns error", func(t *testing.T) {
		t.Parallel()
		client, err := NewProxyHTTPClient("://not-a-url")
		require.Error(t, err)
		require.Nil(t, client)
	})

	t.Run("unsupported scheme returns error", func(t *testing.T) {
		t.Parallel()
		client, err := NewProxyHTTPClient("ftp://proxy:21")
		require.Error(t, err)
		require.Nil(t, client)
	})

	t.Run("ProxyDirect sentinel returns a client with Proxy explicitly nil", func(t *testing.T) {
		t.Parallel()
		client, err := NewProxyHTTPClient(ProxyDirect)
		require.NoError(t, err)
		require.NotNil(t, client)
		transport, ok := client.Transport.(*http.Transport)
		require.True(t, ok, "expected an *http.Transport")
		require.Nil(t, transport.Proxy, "Proxy must be nil'd out, not left unset, so env proxy vars are ignored")
	})
}
