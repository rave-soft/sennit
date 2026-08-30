package discover

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/proxyhttp"
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
const proxyDirect = proxyhttp.Direct

// newProxyHTTPClient builds the discovery client, routed through
// proxyURL. The logic lives in internal/proxyhttp, a leaf package: this
// one is imported by config, so it cannot reach back for config's own
// copy — which is exactly how three copies of it came to exist.
func newProxyHTTPClient(proxyURL string) (*http.Client, error) {
	return proxyhttp.NewClient(proxyURL, 10*time.Second)
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
	// A failed base_url/api_key resolution (e.g. a $(cmd) substitution
	// that errors) must not be swallowed here: doing so used to send the
	// request with the literal, unresolved template as the URL or an
	// empty Authorization header, which the server then answered with a
	// generic 401/connection error that hid the real cause from the user.
	resolvedBase, err := resolver.ResolveValue(baseURL)
	if err != nil {
		return nil, fmt.Errorf("resolve base_url: %w", err)
	}
	resolvedKey, err := resolver.ResolveValue(apiKey)
	if err != nil {
		return nil, fmt.Errorf("resolve api_key: %w", err)
	}

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
		if err != nil {
			// Same rationale as base_url/api_key above: a failing
			// extra_headers substitution (e.g. `$(op read ...)` when the
			// vault is locked) must not be swallowed — that used to send
			// the request without the header at all, and the server
			// answered with the same opaque 401 this function's other
			// resolutions were already fixed to avoid.
			return nil, fmt.Errorf("resolve extra_headers[%s]: %w", k, err)
		}
		if resolved == "" {
			continue
		}
		req.Header.Set(k, resolved)
	}

	return client.Do(req)
}

// fetchJSON issues an authenticated GET request against baseURL+path and
// decodes the JSON response body into a T. It reports ok=false (with T's
// zero value) for any failure along the way -- client construction,
// request execution, a non-200 status, or a decode error -- so enrichers
// can degrade silently to the unenriched model list on any of those,
// exactly as they did before this helper existed.
func fetchJSON[T any](ctx context.Context, cfg Config, resolver Resolver, baseURL, path string) (T, bool) {
	var zero T

	client, err := cfg.httpClient()
	if err != nil {
		return zero, false
	}
	resp, err := doRequest(ctx, client, http.MethodGet, baseURL, path, cfg.APIKey, cfg.ExtraHeaders, resolver, nil)
	if err != nil {
		return zero, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return zero, false
	}

	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return zero, false
	}
	return out, true
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
		if junkModelID(e.ID) {
			// llama.cpp's /v1/models puts the loaded --model path
			// verbatim into "id" instead of a real model name (e.g.
			// "/models/Qwen3.6-...gguf"); a real OpenAI-compatible
			// endpoint never does this. Persisting it would silently
			// clobber a manually configured model list on refresh.
			slog.Warn("Discovery returned a filesystem path instead of a model ID, skipping", "provider", cfg.ID, "id", e.ID)
			continue
		}
		result = append(result, catwalk.Model{
			ID:   e.ID,
			Name: e.ID,
		})
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("discover models for provider %s: endpoint does not expose a usable model list; define models explicitly in the config", cfg.ID)
	}

	return result, nil
}

// junkModelID reports whether id looks like a filesystem path or a GGUF
// file name rather than a real model identifier.
func junkModelID(id string) bool {
	return strings.HasPrefix(id, "/") || strings.HasSuffix(strings.ToLower(id), ".gguf")
}
