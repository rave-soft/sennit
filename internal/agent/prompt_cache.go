package agent

import (
	"maps"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/session"
)

// withPromptCacheKey returns opts with OpenAI's prompt_cache_key set to a
// value stable for one session, so every request a conversation makes is
// routed to the machine already holding that conversation's prefix.
//
// Anthropic-family providers get explicit cache breakpoints (see
// cacheControlOptions); OpenAI's cache needs no breakpoints, but it is a
// per-machine cache, and which machine a request lands on is a routing
// decision the provider makes. prompt_cache_key is how a caller says "these
// requests share a prefix, keep them together". Without it, a long
// conversation re-sends its whole prefix every time routing sends it
// somewhere new — with contexts in the hundreds of thousands of tokens,
// that is the difference between paying for a turn's new messages and
// paying for the entire conversation again.
//
// The key is session.HashID, the same non-identifying digest the
// x-session-affinity header already carries (see sessionHeaders): the same
// question is being asked of the provider, so it gets the same answer.
//
// Config wins. A prompt_cache_key set through provider_options is left
// exactly as written, and so is a provider options value of a type this
// does not recognize — the point is to supply a default, not to overrule
// whoever went to the trouble of choosing one.
func withPromptCacheKey(opts fantasy.ProviderOptions, model Model, providerCfg config.ProviderConfig, sessionID string) fantasy.ProviderOptions {
	if sessionID == "" || providerCfg.Type != openai.Name {
		return opts
	}
	key := session.HashID(sessionID)

	out := fantasy.ProviderOptions{}
	maps.Copy(out, opts)
	switch existing := out[openai.Name].(type) {
	case *openai.ResponsesProviderOptions:
		if existing.PromptCacheKey != nil {
			return opts
		}
		clone := *existing
		clone.PromptCacheKey = &key
		out[openai.Name] = &clone
	case *openai.ProviderOptions:
		if existing.PromptCacheKey != nil {
			return opts
		}
		clone := *existing
		clone.PromptCacheKey = &key
		out[openai.Name] = &clone
	case nil:
		// Nothing configured, so pick the shape the model's own API reads:
		// the two language models both look under openai.Name but assert
		// different concrete types, and one they cannot assert is simply
		// ignored. Guessing wrong therefore costs the optimization, never
		// the request.
		if openai.IsResponsesModel(model.Model.Model()) {
			out[openai.Name] = &openai.ResponsesProviderOptions{PromptCacheKey: &key}
		} else {
			out[openai.Name] = &openai.ProviderOptions{PromptCacheKey: &key}
		}
	default:
		return opts
	}
	return out
}
