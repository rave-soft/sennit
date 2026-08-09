package config

import (
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
}
