package discover

import (
	"context"

	"charm.land/catwalk/pkg/catwalk"
)

// litellmModelInfoResponse mirrors the response from LiteLLM's
// /model/info endpoint, which returns rich metadata including context
// windows, max tokens, and pricing.
type litellmModelInfoResponse struct {
	Data []litellmModelInfo `json:"data"`
}

// litellmModelInfo is a single entry from /model/info.
type litellmModelInfo struct {
	ModelName string           `json:"model_name"`
	ModelInfo litellmModelMeta `json:"model_info"`
}

// litellmModelMeta holds the metadata fields we care about from
// LiteLLM's model_info block.
type litellmModelMeta struct {
	MaxInputTokens     *int64   `json:"max_input_tokens"`
	MaxOutputTokens    *int64   `json:"max_output_tokens"`
	InputCostPerToken  *float64 `json:"input_cost_per_token"`
	OutputCostPerToken *float64 `json:"output_cost_per_token"`
	Mode               string   `json:"mode"`
}

func init() {
	RegisterEnricher("litellm", &litellmEnricher{})
}

// litellmEnricher fetches model metadata from LiteLLM's /model/info
// endpoint and populates context window, max tokens, and pricing on
// discovered models.
type litellmEnricher struct{}

func (e *litellmEnricher) EnrichModels(ctx context.Context, cfg Config, resolver Resolver, models []catwalk.Model) []catwalk.Model {
	infoResp, ok := fetchJSON[litellmModelInfoResponse](ctx, cfg, resolver, stripV1Suffix(cfg.BaseURL), "/model/info")
	if !ok {
		return models
	}
	return applyLitellmMeta(models, infoResp)
}

// applyLitellmMeta maps a decoded /model/info response onto models,
// preserving existing non-zero values (user overrides take precedence).
func applyLitellmMeta(models []catwalk.Model, infoResp litellmModelInfoResponse) []catwalk.Model {
	// Index metadata by model name for O(1) lookup.
	metaByID := make(map[string]litellmModelMeta, len(infoResp.Data))
	for _, entry := range infoResp.Data {
		metaByID[entry.ModelName] = entry.ModelInfo
	}

	return applyModelMeta(models, metaByID, func(model *catwalk.Model, meta litellmModelMeta) {
		if model.ContextWindow == 0 && meta.MaxInputTokens != nil {
			model.ContextWindow = *meta.MaxInputTokens
		}
		if model.DefaultMaxTokens == 0 && meta.MaxOutputTokens != nil {
			model.DefaultMaxTokens = *meta.MaxOutputTokens
		}
		if model.CostPer1MIn == 0 && meta.InputCostPerToken != nil {
			model.CostPer1MIn = *meta.InputCostPerToken * 1_000_000
		}
		if model.CostPer1MOut == 0 && meta.OutputCostPerToken != nil {
			model.CostPer1MOut = *meta.OutputCostPerToken * 1_000_000
		}
	})
}
