package codex

import (
	"net/http"
	"time"

	"github.com/rave-soft/sennit/internal/proxyhttp"
)

// ProxyDirect is the proxy sentinel meaning "no proxy, even if HTTP_PROXY /
// HTTPS_PROXY are set". An empty proxy means "no override — inherit the
// process default", which is what most users want.
//
// It aliases internal/proxyhttp's, the leaf package that owns this logic:
// internal/config imports this package (the catalog entry and the provider
// setup live there), so importing config back would be a cycle — the
// reason this file, config and discover each carried their own copy of the
// same parse-and-validate until proxyhttp existed to hold one.
const ProxyDirect = proxyhttp.Direct

// httpClient builds the client every call in this package makes.
//
// Sign-in has to honour the proxy just as much as the model calls do: for
// a user who can only reach OpenAI through one, a token exchange or model
// list that ignored it would fail while the provider itself looked
// correctly configured.
func httpClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	if proxyURL == "" {
		return &http.Client{Timeout: timeout}, nil
	}
	return proxyhttp.NewClient(proxyURL, timeout)
}

// ValidateProxy reports whether a proxy value is usable, so a UI can reject
// it while the user is still looking at the field rather than at a failed
// sign-in. It delegates to proxyhttp.ValidateProxy, the provider-neutral
// copy of this same check, so codex and any other caller share one rule.
func ValidateProxy(proxyURL string) error {
	return proxyhttp.ValidateProxy(proxyURL)
}
