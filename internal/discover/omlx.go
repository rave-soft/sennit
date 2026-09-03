package discover

import (
	"context"

	"charm.land/catwalk/pkg/catwalk"
)

func init() {
	RegisterEnricher("omlx", &omlxEnricher{})
}

// omlxModelsStatusResponse mirrors the response from oMLX's
// GET /models/status endpoint, which returns model metadata
// including max_context_window and max_tokens.
type omlxModelsStatusResponse struct {
	Models []omlxModelStatus `json:"models"`
}

// omlxModelStatus is a single entry from /v1/models/status.
type omlxModelStatus struct {
	ID               string `json:"id"`
	MaxContextWindow *int64 `json:"max_context_window"`
	MaxTokens        *int64 `json:"max_tokens"`
}

// omlxEnricher fetches model metadata from oMLX's /models/status
// endpoint and populates context window and max tokens on discovered
// models.
type omlxEnricher struct{}

func (e *omlxEnricher) EnrichModels(ctx context.Context, cfg Config, resolver Resolver, models []catwalk.Model) []catwalk.Model {
	// oMLX serves /models/status under the OpenAI-compatible /v1
	// namespace, so the path is relative to the configured base URL
	// (which already includes /v1) rather than the server root.
	statusResp, ok := fetchJSON[omlxModelsStatusResponse](ctx, cfg, resolver, cfg.BaseURL, "/models/status")
	if !ok {
		return models
	}
	return applyOmlxMeta(models, statusResp)
}

// applyOmlxMeta maps a decoded /models/status response onto models,
// preserving existing non-zero values (user overrides take precedence).
func applyOmlxMeta(models []catwalk.Model, statusResp omlxModelsStatusResponse) []catwalk.Model {
	// Index by model ID for O(1) lookup.
	metaByID := make(map[string]omlxModelStatus, len(statusResp.Models))
	for _, m := range statusResp.Models {
		metaByID[m.ID] = m
	}

	// The server is outside our control; a non-positive size isn't a
	// usable value, so only positive numbers overwrite the field.
	return applyModelMeta(models, metaByID, func(model *catwalk.Model, meta omlxModelStatus) {
		if model.ContextWindow == 0 && meta.MaxContextWindow != nil && *meta.MaxContextWindow > 0 {
			model.ContextWindow = *meta.MaxContextWindow
		}
		if model.DefaultMaxTokens == 0 && meta.MaxTokens != nil && *meta.MaxTokens > 0 {
			model.DefaultMaxTokens = *meta.MaxTokens
		}
	})
}
