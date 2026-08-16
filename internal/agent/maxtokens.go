package agent

import "github.com/rave-soft/sennit/internal/oauth/codex"

// modelMaxOutputTokens is the reply cap to send for a model: the explicit
// per-model setting when there is one, otherwise the catalog default. Zero
// means "send none" — callers must omit the field rather than sending a
// zero, which some providers reject.
//
// Codex always reports zero. Its endpoint answers any request carrying the
// field with 400 "Unsupported parameter: max_output_tokens", so it has to be
// omitted regardless of what the catalog or the user's config says — a
// max_tokens someone sets by hand must not turn every request into an error.
// See rejectsMaxOutputTokens.
func modelMaxOutputTokens(model Model) int64 {
	if rejectsMaxOutputTokens(model) {
		return 0
	}
	if model.ModelCfg.MaxTokens != 0 {
		return model.ModelCfg.MaxTokens
	}
	return model.CatalogCfg.DefaultMaxTokens
}

// rejectsMaxOutputTokens reports whether a model's provider refuses the
// output-cap field, so callers that would otherwise substitute a default of
// their own leave it off entirely.
func rejectsMaxOutputTokens(model Model) bool {
	return model.ModelCfg.Provider == codex.ProviderID
}
