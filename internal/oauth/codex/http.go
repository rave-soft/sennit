package codex

import (
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ProxyDirect is the proxy sentinel meaning "no proxy, even if HTTP_PROXY /
// HTTPS_PROXY are set". An empty proxy means "no override — inherit the
// process default", which is what most users want.
//
// This duplicates config.ProxyDirect's literal, and httpClient below
// duplicates config.NewProxyHTTPClient's small parse-and-validate, on
// purpose: internal/config imports this package (the catalog entry and the
// provider setup live there), so importing it back would be a cycle. The
// discover package duplicates the same helper for the same reason.
const ProxyDirect = "none"

// httpClient builds the client every call in this package makes.
//
// Sign-in has to honour the proxy just as much as the model calls do: for
// a user who can only reach OpenAI through one, a token exchange or model
// list that ignored it would fail while the provider itself looked
// correctly configured.
func httpClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	client := &http.Client{Timeout: timeout}
	switch {
	case proxyURL == "":
		return client, nil
	case proxyURL == ProxyDirect:
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		client.Transport = transport
		return client, nil
	}

	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy %q: %w", proxyURL, err)
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("invalid proxy %q: unsupported scheme %q (expected http, https, socks5, or socks5h)", proxyURL, parsed.Scheme)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(parsed)
	client.Transport = transport
	return client, nil
}

// ValidateProxy reports whether a proxy value is usable, so a UI can reject
// it while the user is still looking at the field rather than at a failed
// sign-in.
func ValidateProxy(proxyURL string) error {
	_, err := httpClient(proxyURL, time.Second)
	return err
}
