package discover

import (
	"context"

	"charm.land/catwalk/pkg/catwalk"
)

func init() {
	RegisterEnricher("llamacpp", &llamacppEnricher{})
}

// llamacppModelsResponse mirrors the response from llama-server's
// GET /v1/models endpoint. Unlike the standard OpenAI listing,
// llama-server embeds a meta block with model architecture details.
type llamacppModelsResponse struct {
	Data []llamacppModelEntry `json:"data"`
}

// llamacppModelEntry is a single entry from /v1/models.
type llamacppModelEntry struct {
	ID   string       `json:"id"`
	Meta llamacppMeta `json:"meta"`
}

// llamacppMeta holds per-model architecture metadata exposed by
// llama-server in the /v1/models response.
type llamacppMeta struct {
	NCtx      int64 `json:"n_ctx"`
	NCtxTrain int64 `json:"n_ctx_train"`
	NParams   int64 `json:"n_params"`
	Size      int64 `json:"size"`
}

// llamacppEnricher fetches model metadata from llama-server's
// /v1/models endpoint and populates context window on discovered
// models. It prefers n_ctx (the configured runtime context) and
// falls back to n_ctx_train (the model's trained maximum).
type llamacppEnricher struct{}

func (e *llamacppEnricher) EnrichModels(ctx context.Context, cfg Config, resolver Resolver, models []catwalk.Model) []catwalk.Model {
	modelsResp, ok := fetchJSON[llamacppModelsResponse](ctx, cfg, resolver, cfg.BaseURL, "/v1/models")
	if !ok {
		return models
	}
	return applyLlamacppMeta(models, modelsResp)
}

// applyLlamacppMeta maps a decoded /v1/models response onto models,
// preserving existing non-zero context windows (user overrides take
// precedence).
func applyLlamacppMeta(models []catwalk.Model, modelsResp llamacppModelsResponse) []catwalk.Model {
	// Index by ID for O(1) lookup.
	metaByID := make(map[string]llamacppMeta, len(modelsResp.Data))
	for _, m := range modelsResp.Data {
		metaByID[m.ID] = m.Meta
	}

	return applyModelMeta(models, metaByID, func(model *catwalk.Model, meta llamacppMeta) {
		// Context window: prefer configured n_ctx, fall back to
		// the model's trained maximum.
		if model.ContextWindow == 0 {
			if meta.NCtx > 0 {
				model.ContextWindow = meta.NCtx
			} else if meta.NCtxTrain > 0 {
				model.ContextWindow = meta.NCtxTrain
			}
		}
	})
}
