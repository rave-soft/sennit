package discover

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

func TestOllamaEnricher(t *testing.T) {
	t.Parallel()

	t.Run("populates context window from /api/show", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/api/show", r.URL.Path)
			require.Equal(t, http.MethodPost, r.Method)

			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)

			w.Header().Set("Content-Type", "application/json")
			switch req["model"] {
			case "llama3:latest":
				_ = json.NewEncoder(w).Encode(ollamaShowResponse{
					ModelInfo: map[string]any{
						"llama.context_length": float64(8192),
					},
				})
			case "qwen2:latest":
				_ = json.NewEncoder(w).Encode(ollamaShowResponse{
					ModelInfo: map[string]any{
						"qwen2.context_length": float64(32768),
					},
				})
			default:
				_ = json.NewEncoder(w).Encode(ollamaShowResponse{})
			}
		}))
		defer srv.Close()

		// Base URL includes /v1 (as Sennit configures it); the enricher
		// strips it so /api/show resolves at the server root.
		cfg := Config{ID: "test-ollama", BaseURL: srv.URL + "/v1"}
		models := []catwalk.Model{
			{ID: "llama3:latest", Name: "llama3:latest"},
			{ID: "qwen2:latest", Name: "qwen2:latest"},
			{ID: "unknown:latest", Name: "unknown:latest"},
		}

		e := &ollamaEnricher{}
		result := e.EnrichModels(context.Background(), cfg, &mockResolver{}, models)
		require.Len(t, result, 3)
		require.Equal(t, int64(8192), result[0].ContextWindow)
		require.Equal(t, int64(32768), result[1].ContextWindow)
		require.Equal(t, int64(0), result[2].ContextWindow)
	})

	t.Run("preserves existing context window", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ollamaShowResponse{
				ModelInfo: map[string]any{
					"llama.context_length": float64(8192),
				},
			})
		}))
		defer srv.Close()

		cfg := Config{ID: "test-ollama", BaseURL: srv.URL}
		models := []catwalk.Model{
			{ID: "llama3:latest", ContextWindow: 16384},
		}

		e := &ollamaEnricher{}
		result := e.EnrichModels(context.Background(), cfg, &mockResolver{}, models)
		require.Equal(t, int64(16384), result[0].ContextWindow)
	})

	t.Run("ignores an error response instead of decoding it as a real answer", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// A well-formed body that happens to decode cleanly into
			// ollamaShowResponse, paired with a non-200 status: without a
			// status check this looks exactly like a real answer, so the
			// enricher would wrongly apply it.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(ollamaShowResponse{
				ModelInfo: map[string]any{
					"llama.context_length": float64(8192),
				},
			})
		}))
		defer srv.Close()

		cfg := Config{ID: "test-ollama", BaseURL: srv.URL}
		models := []catwalk.Model{{ID: "llama3:latest", Name: "llama3:latest"}}

		e := &ollamaEnricher{}
		result := e.EnrichModels(context.Background(), cfg, &mockResolver{}, models)
		require.Equal(t, int64(0), result[0].ContextWindow,
			"a non-200 /api/show response must not be decoded as a real model description")
	})

	t.Run("skips /api/show call when context window already set", func(t *testing.T) {
		t.Parallel()
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ollamaShowResponse{})
		}))
		defer srv.Close()

		cfg := Config{ID: "test-ollama", BaseURL: srv.URL}
		models := []catwalk.Model{
			{ID: "m1", ContextWindow: 4096},
			{ID: "m2", ContextWindow: 8192},
		}

		e := &ollamaEnricher{}
		_ = e.EnrichModels(context.Background(), cfg, &mockResolver{}, models)
		require.Equal(t, 0, calls)
	})
}

func TestExtractContextLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		info     map[string]any
		expected int64
	}{
		{"llama key", map[string]any{"llama.context_length": float64(8192)}, 8192},
		{"qwen2 key", map[string]any{"qwen2.context_length": float64(32768)}, 32768},
		{"no context key", map[string]any{"llama.block_count": float64(32)}, 0},
		{"empty map", map[string]any{}, 0},
		{"nil map", nil, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.expected, extractContextLength(tt.info))
		})
	}
}
