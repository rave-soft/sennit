package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/config"
)

// TestCoordinatorWebSearchBackend verifies webSearchBackend reads
// options.web_search from the live config: no section builds cleanly
// (defaulting to the keyless DuckDuckGo scraper), and provider "tavily"
// wires up a backend that actually calls the configured endpoint with the
// shell-expanded api_key.
func TestCoordinatorWebSearchBackend(t *testing.T) {
	t.Run("builds without error when unset", func(t *testing.T) {
		c := newProxyTestCoordinator(t, false)
		backend, err := c.webSearchBackend()
		require.NoError(t, err)
		require.NotNil(t, backend)
	})

	t.Run("tavily provider hits the configured backend with the resolved key", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"results": []map[string]string{{"title": "T", "url": "https://example.com", "content": "c"}},
			})
		}))
		defer srv.Close()

		t.Setenv("BRAID_TEST_WS_KEY", "resolved-key")

		c := newProxyTestCoordinator(t, false)
		c.cfg.Config().Options.WebSearch = &config.WebSearchOptions{
			Provider: "tavily",
			APIKey:   "$BRAID_TEST_WS_KEY",
			BaseURL:  srv.URL,
		}

		backend, err := c.webSearchBackend()
		require.NoError(t, err)

		results, err := backend.Search(context.Background(), "query", 5)
		require.NoError(t, err)
		require.Len(t, results, 1)
		require.Equal(t, "Bearer resolved-key", gotAuth)
	})
}
