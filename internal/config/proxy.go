package config

import (
	"fmt"
	"net/http"
	"net/url"
)

// NewProxyHTTPClient builds an *http.Client whose Transport routes requests
// through proxyURL. An empty proxyURL returns a nil client and nil error,
// signaling the caller to fall back to its default client (which in turn
// falls back to the standard HTTP_PROXY/HTTPS_PROXY/NO_PROXY env behavior).
// http, https, socks5, and socks5h schemes are all supported natively by
// net/http's Transport.Proxy as of Go 1.26 (this repo is on go 1.26.5 per
// go.mod), so no golang.org/x/net/proxy dependency is required.
func NewProxyHTTPClient(proxyURL string) (*http.Client, error) {
	if proxyURL == "" {
		return nil, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy_url %q: %w", proxyURL, err)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("invalid proxy_url %q: unsupported scheme %q (expected http, https, socks5, or socks5h)", proxyURL, u.Scheme)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(u)
	return &http.Client{Transport: transport}, nil
}
