package discover

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"charm.land/catwalk/pkg/catwalk"
)

func init() {
	RegisterEnricher("ollama", &ollamaEnricher{})
}

// ollamaShowResponse mirrors the response from Ollama's POST /api/show
// endpoint. Only the fields we care about are decoded.
type ollamaShowResponse struct {
	ModelInfo map[string]any `json:"model_info"`
}

// ollamaEnricher fetches model metadata from Ollama's /api/show
// endpoint and populates context window on discovered models.
type ollamaEnricher struct{}

func (e *ollamaEnricher) EnrichModels(ctx context.Context, cfg Config, resolver Resolver, models []catwalk.Model) []catwalk.Model {
	// Collect indices that need enrichment.
	var needEnrichment []int
	for i := range models {
		if models[i].ContextWindow == 0 {
			needEnrichment = append(needEnrichment, i)
		}
	}
	if len(needEnrichment) == 0 {
		return models
	}

	client, err := cfg.httpClient()
	if err != nil {
		return models
	}

	// Fetch metadata concurrently with bounded parallelism.
	type result struct {
		index         int
		contextLength int64
	}

	results := make([]result, len(needEnrichment))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5) // Max 5 concurrent requests.

	for ri, idx := range needEnrichment {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()

			resp, err := doRequest(ctx, client, http.MethodPost, stripV1Suffix(cfg.BaseURL), "/api/show",
				cfg.APIKey, cfg.ExtraHeaders, resolver,
				map[string]string{"model": models[idx].ID})
			if err != nil {
				return
			}
			defer resp.Body.Close()

			// An error body decodes to a zero-value ollamaShowResponse,
			// which would otherwise look like a real (empty) model
			// description instead of a failed lookup — skip it, matching
			// fetchJSON's convention elsewhere in this package.
			if resp.StatusCode != http.StatusOK {
				return
			}

			var showResp ollamaShowResponse
			if err := json.NewDecoder(resp.Body).Decode(&showResp); err != nil {
				return
			}

			if cl := extractContextLength(showResp.ModelInfo); cl > 0 {
				results[ri] = result{index: idx, contextLength: cl}
			}
		})
	}
	wg.Wait()

	for _, r := range results {
		if r.contextLength > 0 {
			models[r.index].ContextWindow = r.contextLength
		}
	}

	return models
}

// extractContextLength finds the context_length value in Ollama's
// model_info map. The key is architecture-specific (e.g.
// "llama.context_length", "qwen2.context_length"), so we prefer
// "<general.architecture>.context_length" — model_info's own field naming
// the model's architecture, so this is the model's real context window,
// not an auxiliary one. A model that also exposes a vision tower (e.g.
// "<arch>.vision.context_length", which happens to match the same
// ".context_length" suffix but describes the vision encoder, not the
// model) must not be picked up ahead of it.
//
// If the architecture is missing or its key isn't present, fall back to
// scanning every key ending in ".context_length" and taking the
// lexicographically smallest one — deterministic, unlike ranging over the
// map directly, which returned whichever key Go's randomized iteration
// visited first and could pick the vision context length one run and the
// model's real one the next.
func extractContextLength(info map[string]any) int64 {
	if arch, ok := info["general.architecture"].(string); ok && arch != "" {
		if v, exists := info[arch+".context_length"]; exists {
			if cl, ok := parseContextLength(v); ok {
				return cl
			}
		}
	}

	var bestKey string
	var best int64
	for k, v := range info {
		if !strings.HasSuffix(k, ".context_length") {
			continue
		}
		cl, ok := parseContextLength(v)
		if !ok {
			continue
		}
		if bestKey == "" || k < bestKey {
			bestKey = k
			best = cl
		}
	}
	return best
}

// parseContextLength converts an Ollama model_info value (a JSON number
// decoded as float64, or as json.Number when the caller uses a
// number-preserving decoder) into an int64, reporting ok=false for
// anything else.
func parseContextLength(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}
