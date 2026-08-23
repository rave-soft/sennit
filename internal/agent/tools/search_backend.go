package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/rave-soft/sennit/internal/config"
)

// defaultTavilyBaseURL is Tavily's public search endpoint, used when
// options.web_search.base_url is empty.
const defaultTavilyBaseURL = "https://api.tavily.com/search"

// Tavily can return full page content alongside snippets
// (include_raw_content), which often saves the model a follow-up
// web_fetch round per result. Uncapped, 20 results of raw markdown can
// run to hundreds of KB, so content is budgeted: per result, and in
// total across the response (results past the total budget keep only
// their snippet).
const (
	tavilyRawContentFormat    = "markdown"
	tavilyMaxContentPerResult = 4000
	tavilyMaxContentTotal     = 20000
)

// tavilyBackend calls the Tavily Search API
// (https://docs.tavily.com/documentation/api-reference/endpoint/search).
// It was chosen over Brave Search because its request/response shape is a
// single JSON POST with a flat results[].title/url/content array — no
// separate web/news/video result buckets to reconcile with SearchResult,
// and Bearer-token auth matches the pattern already used for LLM
// providers.
type tavilyBackend struct {
	client  *http.Client
	apiKey  string
	baseURL string
}

type tavilySearchRequest struct {
	Query             string `json:"query"`
	MaxResults        int    `json:"max_results,omitempty"`
	IncludeRawContent string `json:"include_raw_content,omitempty"`
}

type tavilySearchResult struct {
	Title      string `json:"title"`
	URL        string `json:"url"`
	Content    string `json:"content"`
	RawContent string `json:"raw_content"`
}

type tavilySearchResponse struct {
	Results []tavilySearchResult `json:"results"`
}

func (b *tavilyBackend) Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	if b.apiKey == "" {
		return nil, fmt.Errorf("tavily search: no API key configured (set options.web_search.api_key)")
	}

	baseURL := b.baseURL
	if baseURL == "" {
		baseURL = defaultTavilyBaseURL
	}

	body, err := json.Marshal(tavilySearchRequest{
		Query:             query,
		MaxResults:        maxResults,
		IncludeRawContent: tavilyRawContentFormat,
	})
	if err != nil {
		return nil, fmt.Errorf("tavily search: encoding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("tavily search: creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.apiKey)

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tavily search: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("tavily search: reading response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("tavily search: authentication failed, check options.web_search.api_key (status %d)", resp.StatusCode)
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("tavily search: rate limited or quota exceeded (status 429)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tavily search: unexpected status %d", resp.StatusCode)
	}

	var parsed tavilySearchResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("tavily search: decoding response: %w", err)
	}

	if maxResults > 0 && len(parsed.Results) > maxResults {
		parsed.Results = parsed.Results[:maxResults]
	}
	results := make([]SearchResult, 0, len(parsed.Results))
	contentBudget := tavilyMaxContentTotal
	for i, r := range parsed.Results {
		// A budget of zero or less means nothing is left to spend on this
		// result (mirrors the old truncateUTF8's limit<=0 special case:
		// drop the content rather than cut it to an empty, marker-only
		// string).
		limit := min(tavilyMaxContentPerResult, contentBudget)
		content := ""
		if limit > 0 {
			content = truncateToRuneBoundary(r.RawContent, limit)
			if len(content) < len(r.RawContent) {
				content += "\n… [content truncated]"
			}
		}
		contentBudget -= len(content)
		results = append(results, SearchResult{
			Title:    r.Title,
			Link:     r.URL,
			Snippet:  r.Content,
			Position: i + 1,
			Content:  content,
		})
	}
	return results, nil
}

// NewSearchBackend builds the SearchBackend selected by opts. A zero-value
// opts (or a nil *WebSearchOptions upstream) yields the default DuckDuckGo
// backend. resolver runs opts.APIKey/opts.ProxyURL through shell expansion,
// the same as provider api_key/proxy_url; pass config.IdentityResolver() to
// skip expansion. defaultClient is used as-is for DuckDuckGo/Tavily unless
// opts.ProxyURL requests a different transport.
func NewSearchBackend(opts config.WebSearchOptions, resolver config.VariableResolver, defaultClient *http.Client) (SearchBackend, error) {
	if resolver == nil {
		resolver = config.IdentityResolver()
	}

	client := defaultClient
	if opts.ProxyURL != "" {
		proxyURL, err := resolver.ResolveValue(opts.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("options.web_search.proxy_url: %w", err)
		}
		proxyClient, err := config.NewProxyHTTPClient(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("options.web_search: %w", err)
		}
		if proxyClient != nil {
			client = proxyClient
		}
	}
	if client == nil {
		client = defaultSearchHTTPClient()
	}

	provider := opts.Provider
	if provider == "" {
		provider = "duckduckgo"
	}

	switch provider {
	case "duckduckgo":
		return &duckDuckGoBackend{client: client}, nil
	case "tavily":
		apiKey := opts.APIKey
		if apiKey != "" {
			resolved, err := resolver.ResolveValue(apiKey)
			if err != nil {
				return nil, fmt.Errorf("options.web_search.api_key: %w", err)
			}
			apiKey = resolved
		}
		return &tavilyBackend{client: client, apiKey: apiKey, baseURL: opts.BaseURL}, nil
	default:
		return nil, fmt.Errorf("unknown options.web_search.provider %q (expected \"duckduckgo\" or \"tavily\")", opts.Provider)
	}
}
