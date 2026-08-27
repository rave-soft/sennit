package discover

import (
	"context"

	"charm.land/catwalk/pkg/catwalk"
)

func init() {
	RegisterEnricher("lmstudio", &lmstudioEnricher{})
}

// lmstudioModelsResponse mirrors the response from LM Studio's native
// GET /api/v1/models endpoint. The model array is returned under the
// "models" key (not "data" like the OpenAI-compatible endpoint). Only
// the fields we care about are decoded.
type lmstudioModelsResponse struct {
	Models []lmstudioModelEntry `json:"models"`
}

// lmstudioModelEntry is a single entry from /api/v1/models.
type lmstudioModelEntry struct {
	Key              string               `json:"key"`
	DisplayName      string               `json:"display_name"`
	MaxContextLength int64                `json:"max_context_length"`
	LoadedInstances  []lmstudioInstance   `json:"loaded_instances"`
	Capabilities     lmstudioCapabilities `json:"capabilities"`
}

// lmstudioCapabilities holds optional model capability flags from
// LM Studio's /api/v1/models endpoint.
type lmstudioCapabilities struct {
	Vision bool `json:"vision"`
}

// lmstudioInstance is a currently loaded model instance with its
// runtime config.
type lmstudioInstance struct {
	Config lmstudioInstanceConfig `json:"config"`
}

// lmstudioInstanceConfig holds per-instance runtime settings.
type lmstudioInstanceConfig struct {
	ContextLength int64 `json:"context_length"`
}

// lmstudioEnricher fetches model metadata from LM Studio's native
// /api/v1/models endpoint and populates context window, display name,
// and vision support on discovered models.
type lmstudioEnricher struct{}

func (e *lmstudioEnricher) EnrichModels(ctx context.Context, cfg Config, resolver Resolver, models []catwalk.Model) []catwalk.Model {
	modelsResp, ok := fetchJSON[lmstudioModelsResponse](ctx, cfg, resolver, stripV1Suffix(cfg.BaseURL), "/api/v1/models")
	if !ok {
		return models
	}
	return applyLmstudioMeta(cfg, models, modelsResp)
}

// applyLmstudioMeta maps a decoded /api/v1/models response onto models,
// preserving existing non-zero fields (user overrides take precedence).
func applyLmstudioMeta(cfg Config, models []catwalk.Model, modelsResp lmstudioModelsResponse) []catwalk.Model {
	// Index by key for O(1) lookup.
	metaByKey := make(map[string]lmstudioModelEntry, len(modelsResp.Models))
	for _, m := range modelsResp.Models {
		metaByKey[m.Key] = m
	}

	// IDs the user already listed (by hand, or from an earlier discovery
	// merged into config). SupportsImages is a plain bool with no "unset"
	// sentinel to gate on the way ContextWindow (0) and Name (== ID) do
	// below, so it needs this explicit membership check instead: without
	// it, enrichment would unconditionally overwrite an existing model's
	// SupportsImages on every call, clobbering a user override with
	// whatever this LM Studio instance currently reports.
	existing := make(map[string]struct{}, len(cfg.ExistingModels))
	for _, m := range cfg.ExistingModels {
		existing[m.ID] = struct{}{}
	}

	return applyModelMeta(models, metaByKey, func(model *catwalk.Model, meta lmstudioModelEntry) {
		// Context window: prefer loaded instance config, fall back
		// to the model-level max.
		if model.ContextWindow == 0 {
			if len(meta.LoadedInstances) > 0 && meta.LoadedInstances[0].Config.ContextLength > 0 {
				model.ContextWindow = meta.LoadedInstances[0].Config.ContextLength
			} else if meta.MaxContextLength > 0 {
				model.ContextWindow = meta.MaxContextLength
			}
		}

		// Display name if not already set by user.
		if model.Name == model.ID && meta.DisplayName != "" {
			model.Name = meta.DisplayName
		}

		// Vision support from capabilities -- only for models the user
		// didn't already specify; see the Enricher contract note on
		// existing above.
		if _, userSpecified := existing[model.ID]; !userSpecified {
			model.SupportsImages = meta.Capabilities.Vision
		}
	})
}
