package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/testenv"
)

// TestNewSearchBackendDefaultsToDuckDuckGo verifies that an empty
// options.web_search section (and an explicit "duckduckgo" provider)
// resolve to the keyless scraper.
func TestNewSearchBackendDefaultsToDuckDuckGo(t *testing.T) {
	backend, err := NewSearchBackend(config.WebSearchOptions{}, nil, nil)
	require.NoError(t, err)
	require.IsType(t, &duckDuckGoBackend{}, backend)

	backend, err = NewSearchBackend(config.WebSearchOptions{Provider: "duckduckgo"}, nil, nil)
	require.NoError(t, err)
	require.IsType(t, &duckDuckGoBackend{}, backend)
}

// TestNewSearchBackendUnknownProvider verifies an unrecognized provider
// name is a config error, not a silent fallback.
func TestNewSearchBackendUnknownProvider(t *testing.T) {
	_, err := NewSearchBackend(config.WebSearchOptions{Provider: "bing"}, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bing")
}

// TestTavilyBackendSearch drives the Tavily backend against an httptest
// mock, asserting the request shape (POST JSON, Bearer auth) and that the
// response's results[].title/url/content map onto SearchResult.
func TestTavilyBackendSearch(t *testing.T) {
	var gotAuth, gotMethod, gotContentType string
	var gotBody struct {
		Query             string `json:"query"`
		MaxResults        int    `json:"max_results"`
		IncludeRawContent string `json:"include_raw_content"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]string{
				{"title": "Sennit", "url": "https://example.com/sennit", "content": "A terminal AI agent.", "raw_content": "# Sennit\n\nFull page text."},
				{"title": "Other", "url": "https://example.com/other", "content": "Something else."},
			},
		})
	}))
	defer srv.Close()

	backend, err := NewSearchBackend(config.WebSearchOptions{
		Provider: "tavily",
		APIKey:   "test-key",
		BaseURL:  srv.URL,
	}, nil, nil)
	require.NoError(t, err)

	results, err := backend.Search(context.Background(), "sennit coding agent", 5)
	require.NoError(t, err)

	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, "application/json", gotContentType)
	require.Equal(t, "Bearer test-key", gotAuth)
	require.Equal(t, "sennit coding agent", gotBody.Query)
	require.Equal(t, 5, gotBody.MaxResults)
	require.Equal(t, "markdown", gotBody.IncludeRawContent)

	require.Len(t, results, 2)
	require.Equal(t, "Sennit", results[0].Title)
	require.Equal(t, "https://example.com/sennit", results[0].Link)
	require.Equal(t, "A terminal AI agent.", results[0].Snippet)
	require.Equal(t, "# Sennit\n\nFull page text.", results[0].Content)
	require.Equal(t, 1, results[0].Position)
	require.Empty(t, results[1].Content)
}

// TestTavilyBackendContentBudget verifies raw_content is truncated per
// result and stops entirely once the total budget across the response is
// spent, so a page-heavy search can't flood the model's context.
func TestTavilyBackendContentBudget(t *testing.T) {
	big := strings.Repeat("x", tavilyMaxContentPerResult+1000)
	nResults := tavilyMaxContentTotal/tavilyMaxContentPerResult + 2
	pages := make([]map[string]string, 0, nResults)
	for i := range nResults {
		pages = append(pages, map[string]string{
			"title":       fmt.Sprintf("Page %d", i),
			"url":         fmt.Sprintf("https://example.com/%d", i),
			"content":     "snippet",
			"raw_content": big,
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": pages})
	}))
	defer srv.Close()

	backend, err := NewSearchBackend(config.WebSearchOptions{
		Provider: "tavily",
		APIKey:   "test-key",
		BaseURL:  srv.URL,
	}, nil, nil)
	require.NoError(t, err)

	results, err := backend.Search(context.Background(), "query", nResults)
	require.NoError(t, err)
	require.Len(t, results, nResults)

	total := 0
	for _, r := range results {
		require.LessOrEqual(t, len(r.Content), tavilyMaxContentPerResult+len("\n… [content truncated]"))
		total += len(r.Content)
	}
	require.LessOrEqual(t, total, tavilyMaxContentTotal+len("\n… [content truncated]"))

	require.Contains(t, results[0].Content, "[content truncated]")
	require.Empty(t, results[len(results)-1].Content)
	for _, r := range results {
		require.Equal(t, "snippet", r.Snippet)
	}
}

// TestTruncateUTF8 verifies truncation never splits a multi-byte rune.
func TestTruncateUTF8(t *testing.T) {
	require.Equal(t, "short", truncateUTF8("short", 10))
	require.Equal(t, "", truncateUTF8("anything", 0))

	s := strings.Repeat("я", 10) // 2 bytes per rune
	cut := truncateUTF8(s, 5)
	require.True(t, utf8.ValidString(cut))
	require.Equal(t, strings.Repeat("я", 2)+"\n… [content truncated]", cut)
}

// TestTavilyBackendAuthError verifies a 401/403 from the API surfaces a
// message pointing at api_key rather than a raw HTTP error.
func TestTavilyBackendAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	backend, err := NewSearchBackend(config.WebSearchOptions{
		Provider: "tavily",
		APIKey:   "bad-key",
		BaseURL:  srv.URL,
	}, nil, nil)
	require.NoError(t, err)

	_, err = backend.Search(context.Background(), "query", 5)
	require.Error(t, err)
	require.Contains(t, err.Error(), "api_key")
}

// TestTavilyBackendRequiresAPIKey verifies a missing key fails fast with a
// model-readable error instead of sending an unauthenticated request.
func TestTavilyBackendRequiresAPIKey(t *testing.T) {
	backend, err := NewSearchBackend(config.WebSearchOptions{Provider: "tavily"}, nil, nil)
	require.NoError(t, err)

	_, err = backend.Search(context.Background(), "query", 5)
	require.Error(t, err)
	require.Contains(t, err.Error(), "api_key")
}

// TestNewSearchBackendExpandsAPIKey verifies options.web_search.api_key
// runs through shell expansion, the same as provider api_key.
func TestNewSearchBackendExpandsAPIKey(t *testing.T) {
	t.Setenv("SENNIT_TEST_TAVILY_KEY", "expanded-key")
	resolver := config.NewShellVariableResolver(testenv.New(map[string]string{
		"SENNIT_TEST_TAVILY_KEY": "expanded-key",
	}))

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]string{}})
	}))
	defer srv.Close()

	backend, err := NewSearchBackend(config.WebSearchOptions{
		Provider: "tavily",
		APIKey:   "$SENNIT_TEST_TAVILY_KEY",
		BaseURL:  srv.URL,
	}, resolver, nil)
	require.NoError(t, err)

	_, err = backend.Search(context.Background(), "query", 5)
	require.NoError(t, err)
	require.Equal(t, "Bearer expanded-key", gotAuth)
}

// TestWebSearchToolUsesConfiguredBackend verifies the web_search tool
// delegates to whatever SearchBackend it's given, so a Tavily
// configuration is actually reached instead of always scraping DuckDuckGo.
func TestWebSearchToolUsesConfiguredBackend(t *testing.T) {
	stub := &stubSearchBackend{results: []SearchResult{{Title: "Stubbed", Link: "https://example.com", Snippet: "stub", Position: 1}}}
	tool := NewWebSearchTool(nil, t.TempDir(), nil, stub)

	resp, err := runWebSearchTool(t, tool, context.Background(), WebSearchParams{Query: "anything"})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Stubbed")
	require.True(t, stub.called)
}

type stubSearchBackend struct {
	called  bool
	results []SearchResult
	err     error
}

func (s *stubSearchBackend) Search(_ context.Context, _ string, _ int) ([]SearchResult, error) {
	s.called = true
	return s.results, s.err
}

// TestWebSearchDescriptionMatchesBackend verifies the tool description
// names the selected backend and only tells the model to follow up with
// web_fetch when results are snippet-only.
func TestWebSearchDescriptionMatchesBackend(t *testing.T) {
	ddg := NewWebSearchTool(nil, t.TempDir(), nil, &duckDuckGoBackend{})
	require.Contains(t, ddg.Info().Description, "DuckDuckGo")
	require.Contains(t, ddg.Info().Description, "Follow up with web_fetch")

	tavily := NewWebSearchTool(nil, t.TempDir(), nil, &tavilyBackend{})
	require.Contains(t, tavily.Info().Description, "Tavily")
	require.Contains(t, tavily.Info().Description, "page content")
	require.NotContains(t, tavily.Info().Description, "Follow up with web_fetch")

	stub := NewWebSearchTool(nil, t.TempDir(), nil, &stubSearchBackend{})
	require.Contains(t, stub.Info().Description, "the configured search provider")
}
