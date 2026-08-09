package discover

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
)

// httpClient is shared across all discovery and enrichment calls. It
// has a reasonable timeout so individual requests cannot block forever
// even if the caller forgets to set a context deadline.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// proxyDirect duplicates config.ProxyDirect's literal value. This
// intentionally duplicates config.NewProxyHTTPClient's small
// parse-and-validate logic instead of importing internal/config: config
// already builds discover.Config (see load.go's discoverCustomProviderModels),
// so importing config from here would create an import cycle, and pulling in
// the whole config package just for this one helper isn't warranted anyway.
const proxyDirect = "none"

// newProxyHTTPClient builds an *http.Client whose Transport routes requests
// through proxyURL. proxyURL == proxyDirect returns a client whose Transport
// has Proxy explicitly nil'd out, forcing a direct connection even when
// HTTP_PROXY/HTTPS_PROXY are set. http, https, socks5, and socks5h schemes
// are all supported natively by net/http's Transport.Proxy.
func newProxyHTTPClient(proxyURL string) (*http.Client, error) {
	if proxyURL == proxyDirect {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		return &http.Client{Timeout: 10 * time.Second, Transport: transport}, nil
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
	return &http.Client{Timeout: 10 * time.Second, Transport: transport}, nil
}

// httpClient returns the client to use for requests against this provider:
// the shared package-level client normally (so discovery and enrichment
// calls across providers reuse connections), or a dedicated proxying client
// when ProxyURL is set.
func (c Config) httpClient() (*http.Client, error) {
	if c.ProxyURL == "" {
		return httpClient, nil
	}
	client, err := newProxyHTTPClient(c.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("provider %s: proxy_url: %w", c.ID, err)
	}
	return client, nil
}

// stripV1Suffix removes a trailing /v1 from a base URL. Enricher
// endpoints (e.g. Ollama's /api/show, LM Studio's /api/v1/models) are
// served at the server root, not under the OpenAI-compatible /v1
// prefix. Since provider configs typically include /v1 in the base URL
// for chat completions, enrichers must strip it before constructing
// their own request paths.
func stripV1Suffix(baseURL string) string {
	return strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
}

// doRequest builds and executes an authenticated HTTP request using client.
// It resolves variable references in the base URL, API key, and extra
// headers via the provided Resolver. The path is joined to the base URL
// with proper slash handling.
func doRequest(ctx context.Context, client *http.Client, method, baseURL, path, apiKey string, extraHeaders map[string]string, resolver Resolver, body any) (*http.Response, error) {
	resolvedBase, _ := resolver.ResolveValue(baseURL)
	resolvedKey, _ := resolver.ResolveValue(apiKey)

	url := strings.TrimRight(resolvedBase, "/") + "/" + strings.TrimLeft(path, "/")

	var reqBody *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling request body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	var req *http.Request
	var err error
	if reqBody != nil {
		req, err = http.NewRequestWithContext(ctx, method, url, reqBody)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url, nil)
	}
	if err != nil {
		return nil, err
	}

	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if resolvedKey != "" {
		req.Header.Set("Authorization", "Bearer "+resolvedKey)
	}
	for k, v := range extraHeaders {
		resolved, err := resolver.ResolveValue(v)
		if err != nil || resolved == "" {
			continue
		}
		req.Header.Set(k, resolved)
	}

	return client.Do(req)
}

// Config holds the provider configuration needed for model discovery.
type Config struct {
	ID           string
	BaseURL      string
	APIKey       string
	ExtraHeaders map[string]string
	// Existing models from config — IDs present in this list are skipped
	// during discovery (user-specified models win).
	ExistingModels []catwalk.Model
	// ProxyURL, if set, routes discovery requests through this proxy
	// instead of the shared package-level httpClient. Already resolved
	// (shell-expanded) by the caller. Empty means use default client
	// behavior (env HTTP_PROXY etc, via the shared client).
	ProxyURL string
}

// Resolver resolves variable references (e.g. $ENV_VAR) in config values.
type Resolver interface {
	ResolveValue(val string) (string, error)
}

type modelsResponse struct {
	Data []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// DiscoverModels fetches available models from the provider's /models endpoint.
// It uses the provided context for cancellation and timeout; callers should set
// a deadline (e.g. context.WithTimeout) to avoid blocking indefinitely.
// Models whose IDs already appear in cfg.ExistingModels are skipped —
// user-specified models take precedence.
func DiscoverModels(ctx context.Context, cfg Config, resolver Resolver) ([]catwalk.Model, error) {
	client, err := cfg.httpClient()
	if err != nil {
		return nil, fmt.Errorf("discover models for provider %s: %w", cfg.ID, err)
	}

	resp, err := doRequest(ctx, client, http.MethodGet, cfg.BaseURL, "/models", cfg.APIKey, cfg.ExtraHeaders, resolver, nil)
	if err != nil {
		return nil, fmt.Errorf("discover models for provider %s: %w", cfg.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discover models for provider %s: %s", cfg.ID, resp.Status)
	}

	var modelsResp modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		return nil, fmt.Errorf("discover models for provider %s: %w", cfg.ID, err)
	}

	// Build set of existing model IDs to skip.
	existing := make(map[string]struct{}, len(cfg.ExistingModels))
	for _, m := range cfg.ExistingModels {
		existing[m.ID] = struct{}{}
	}

	// Start with user-specified models.
	result := make([]catwalk.Model, len(cfg.ExistingModels))
	copy(result, cfg.ExistingModels)

	// Append discovered models not already in the list.
	for _, e := range modelsResp.Data {
		if _, ok := existing[e.ID]; ok {
			continue
		}
		result = append(result, catwalk.Model{
			ID:   e.ID,
			Name: e.ID,
		})
	}

	return result, nil
}
