package agent

import (
	"net/http"
	"strings"

	"charm.land/fantasy"
)

// streamErrorClass is what classifyStreamError decides a provider error
// means for handleStreamError's persisted finish reason: either a known,
// specially-handled condition, or the generic provider-error fallback.
type streamErrorClass int

const (
	// classGenericProviderError is any provider error handleStreamError
	// does not recognize as a specific condition; it persists the
	// provider's own Title/Message as the finish reason.
	classGenericProviderError streamErrorClass = iota
	// classModelNotEnabled means the account has not enabled the
	// requested model with the provider (e.g. a model not yet turned on
	// in GitHub Copilot's settings). handleStreamError turns this into a
	// typed ProviderQuotaError instead of a generic finish reason so the
	// TUI can render its own styled hyperlink.
	classModelNotEnabled
	// classRateLimited means the provider answered 429 and kept answering
	// it until the step's retry budget ran out - with account rotation, if
	// the provider rotates, having already had its turn from inside that
	// budget. The turn is over either way; this only decides what the user
	// is told, because a provider's own 429 body is usually empty or
	// unhelpful ("too many requests") and reads as a bug rather than as
	// "wait a while".
	classRateLimited
)

// modelNotEnabledPhrases are lowercase substrings known to appear in a
// provider's free-text error message when it is rejecting a request
// because the model is not enabled for the account, rather than for some
// other reason (bad request, rate limit, etc.).
//
// Providers report this condition as prose, not a structured error code,
// so matching is substring-based against a short, documented list instead
// of one exact string. A single `==` comparison against Copilot's exact
// wording ("The requested model is not supported.") broke the moment the
// provider capitalized, punctuated, or reworded that sentence differently
// than the one response this was written against; a provider is free to do
// that without notice since the string is not part of any API contract.
// Add a phrase here, in lowercase, when a provider is seen using new
// wording for the same condition - classifyStreamError's table test is the
// place to pin the new phrase down.
var modelNotEnabledPhrases = []string{
	"requested model is not supported",
	"model is not enabled",
	"model not enabled",
}

// classifyStreamError inspects a non-cancel Stream error and reports which
// specific condition, if any, it matches. providerErr may be nil (the
// error wasn't a *fantasy.ProviderError at all), in which case it is
// always the generic class.
func classifyStreamError(providerErr *fantasy.ProviderError) streamErrorClass {
	if providerErr == nil {
		return classGenericProviderError
	}
	if providerErr.StatusCode == http.StatusTooManyRequests {
		return classRateLimited
	}
	msg := strings.ToLower(providerErr.Message)
	for _, phrase := range modelNotEnabledPhrases {
		if strings.Contains(msg, phrase) {
			return classModelNotEnabled
		}
	}
	return classGenericProviderError
}
