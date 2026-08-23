package config

import (
	"net/http"

	"github.com/rave-soft/sennit/internal/proxyhttp"
)

// ProxyDirect is the proxy_url sentinel value meaning "no proxy, even if
// HTTP_PROXY/HTTPS_PROXY are set in the environment." Distinct from an
// empty proxy_url, which means "no explicit override — inherit the
// process's default proxy behavior (env vars via net/http)."
const ProxyDirect = proxyhttp.Direct

// NewProxyHTTPClient builds an *http.Client whose Transport routes requests
// through proxyURL. An empty proxyURL returns a nil client and nil error,
// signaling the caller to fall back to its default client (which in turn
// falls back to the standard HTTP_PROXY/HTTPS_PROXY/NO_PROXY env behavior).
// proxyURL == ProxyDirect returns a client whose Transport has Proxy
// explicitly nil'd out, forcing a direct connection even when those env
// vars are set. http, https, socks5, and socks5h schemes are all supported
// natively by net/http's Transport.Proxy as of Go 1.26 (this repo is on go
// 1.26.5 per go.mod), so no golang.org/x/net/proxy dependency is required.
func NewProxyHTTPClient(proxyURL string) (*http.Client, error) {
	if proxyURL == "" {
		return nil, nil
	}
	// No timeout: this client is used for streaming model responses,
	// which run far longer than any fixed deadline would allow.
	return proxyhttp.NewClient(proxyURL, 0)
}
