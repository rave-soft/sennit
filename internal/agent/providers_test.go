package agent

import (
	"net/http"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/log"
	"github.com/stretchr/testify/require"
)

// newProxyTestCoordinator returns a coordinator with just enough state to
// exercise buildProviderHTTPClient / buildProvider: those only touch c.cfg,
// so a bare coordinator wrapping a real config store is enough. Named
// distinctly from coordinator_test.go's newTestCoordinator, which serves a
// different (env/providerCfg-based) purpose.
func newProxyTestCoordinator(t *testing.T, debug bool) *coordinator {
	t.Helper()
	cfg, err := configruntime.Load(t.TempDir(), t.TempDir(), debug)
	require.NoError(t, err)
	return &coordinator{cfg: cfg}
}

func TestBuildProviderHTTPClient(t *testing.T) {
	t.Run("no proxy, no debug returns nil client", func(t *testing.T) {
		c := newProxyTestCoordinator(t, false)
		client, err := c.buildProviderHTTPClient("")
		require.NoError(t, err)
		require.Nil(t, client)
	})

	t.Run("debug only wraps default transport in logger", func(t *testing.T) {
		c := newProxyTestCoordinator(t, true)
		client, err := c.buildProviderHTTPClient("")
		require.NoError(t, err)
		require.NotNil(t, client)
		logger, ok := client.Transport.(*log.HTTPRoundTripLogger)
		require.True(t, ok, "expected transport to be wrapped in HTTPRoundTripLogger")
		require.Equal(t, http.DefaultTransport, logger.Transport)
	})

	t.Run("proxy only returns a proxying client with no logger", func(t *testing.T) {
		c := newProxyTestCoordinator(t, false)
		client, err := c.buildProviderHTTPClient("http://localhost:8080")
		require.NoError(t, err)
		require.NotNil(t, client)
		_, ok := client.Transport.(*log.HTTPRoundTripLogger)
		require.False(t, ok, "proxy-only client should not be wrapped in a logger")
		_, ok = client.Transport.(*http.Transport)
		require.True(t, ok, "expected the proxying *http.Transport")
	})

	t.Run("proxy and debug compose: logger wraps the proxying transport", func(t *testing.T) {
		c := newProxyTestCoordinator(t, true)
		client, err := c.buildProviderHTTPClient("http://localhost:8080")
		require.NoError(t, err)
		require.NotNil(t, client)
		logger, ok := client.Transport.(*log.HTTPRoundTripLogger)
		require.True(t, ok, "expected transport to be wrapped in HTTPRoundTripLogger")
		_, ok = logger.Transport.(*http.Transport)
		require.True(t, ok, "expected the logger to wrap the proxying *http.Transport")
	})

	t.Run("invalid proxy_url surfaces an error", func(t *testing.T) {
		c := newProxyTestCoordinator(t, false)
		client, err := c.buildProviderHTTPClient("://not-a-url")
		require.Error(t, err)
		require.Nil(t, client)
	})

	t.Run("ProxyDirect returns a real client with Proxy explicitly nil", func(t *testing.T) {
		c := newProxyTestCoordinator(t, false)
		client, err := c.buildProviderHTTPClient(config.ProxyDirect)
		require.NoError(t, err)
		// Distinct from the "no proxyURL, no debug" case above, which
		// returns a nil *http.Client entirely: "none" must still produce a
		// real client, just one whose Transport ignores env proxy vars.
		require.NotNil(t, client)
		transport, ok := client.Transport.(*http.Transport)
		require.True(t, ok, "expected the direct *http.Transport")
		require.Nil(t, transport.Proxy, "Proxy must be nil'd out, not left unset")
	})
}
