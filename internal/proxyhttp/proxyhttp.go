// Package proxyhttp builds HTTP clients that route through a configured
// proxy.
//
// It is a leaf: it imports nothing of Sennit's, which is the whole point.
// Three packages needed this logic and none of them could import another
// without a cycle — internal/config (the configuration owner),
// internal/oauth/codex and internal/discover (both imported *by* config) —
// so each grew its own copy, each with a comment explaining the cycle it
// was working around. The copies had already drifted: two of them set a
// ten-second timeout and one set none.
package proxyhttp

import (
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Direct is the proxy_url value that means "no proxy": a client built for
// it ignores HTTP_PROXY/HTTPS_PROXY and connects straight out.
const Direct = "none"

// NewClient builds an *http.Client whose Transport routes requests through
// proxyURL, with timeout as the client's overall deadline (zero for none).
//
// proxyURL == [Direct] returns a client whose Transport has Proxy
// explicitly nil'd out, forcing a direct connection even when the
// environment names a proxy. http, https, socks5 and socks5h are all
// supported natively by net/http's Transport.Proxy.
// ValidateProxy reports whether proxyURL is usable, so a UI can reject it
// while the user is still looking at the field rather than at a failed
// request later. "" (inherit) and [Direct] both validate cleanly; anything
// else goes through the same parse-and-scheme-check NewClient does.
func ValidateProxy(proxyURL string) error {
	if proxyURL == "" {
		return nil
	}
	_, err := NewClient(proxyURL, time.Second)
	return err
}

func NewClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	if proxyURL == Direct {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		return &http.Client{Timeout: timeout, Transport: transport}, nil
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
	return &http.Client{Timeout: timeout, Transport: transport}, nil
}
