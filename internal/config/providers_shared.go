package config

import (
	"fmt"
	"log/slog"
)

// resolveProviderHeaders resolves every value in headers (in place, via
// resolver's $(...) / $VAR substitution) and drops any header that resolves
// to the empty string (unset bare $VAR under lenient nounset, $(echo), or a
// literal ""). A failing $(...) aborts the whole provider load, matching
// the MCP header error contract: mergeCatalogProviders and
// validateCustomProviders both propagate this error straight out of config
// loading rather than dropping the provider or warning, so this helper
// takes no logging/Problem parameters — there is nothing to make
// configurable, both call sites already agree.
func resolveProviderHeaders(headers map[string]string, resolver VariableResolver, providerID string) error {
	for k, v := range headers {
		resolved, err := resolver.ResolveValue(v)
		if err != nil {
			return fmt.Errorf("resolving provider %s header %q: %w", providerID, k, err)
		}
		if resolved == "" {
			delete(headers, k)
			continue
		}
		headers[k] = resolved
	}
	return nil
}

// resolveOptionalProxy resolves a provider's proxy_url, if set. Unlike
// api_key/base_url, the proxy is purely optional: a failed or empty
// resolution must never block the provider from loading, so this only
// warns and returns "" instead of returning an error. Both
// mergeCatalogProviders and validateCustomProviders agree on this, so
// (unlike dropProvider) there is no per-call-site parameter here.
func resolveOptionalProxy(proxyURL string, resolver VariableResolver, providerID string) string {
	if proxyURL == "" {
		return ""
	}
	resolved, err := resolver.ResolveValue(proxyURL)
	if err != nil || resolved == "" {
		slog.Warn("Ignoring provider proxy_url due to resolution failure", "provider", providerID, "error", err)
		return ""
	}
	return resolved
}

// dropProvider removes a provider from the config, logging and (optionally)
// recording a doctor Problem about why.
//
// The two callers disagree, sometimes deliberately, on how loudly to
// report a drop, so both are left as explicit parameters instead of being
// collapsed to one behavior:
//   - log is the slog level to use (slog.Warn everywhere except the
//     disable_flag case in validateCustomProviders, which intentionally
//     logs at slog.Debug and passes a nil problem: the user asked for that
//     provider to be off, so it is not something `sennit doctor` should
//     flag as a problem).
//   - problem, when non-nil, is recorded via c.addProblem so `sennit
//     doctor` and the TUI's /doctor dialog surface the drop. A nil problem
//     reproduces two known call sites that don't currently report one; see
//     providers_validate.go's discovery-failure branch, which is flagged
//     in the refactor notes as likely accidental drift rather than a
//     deliberate choice like the disable-flag case.
func (c *Config) dropProvider(id string, log func(msg string, args ...any), logMsg string, logArgs []any, problem *Problem) {
	log(logMsg, logArgs...)
	if problem != nil {
		c.addProblem(*problem)
	}
	c.Providers.Del(id)
}

// providerProblem is the shared constructor for every provider-related
// Problem in providers_merge.go and providers_validate.go: Severity and
// Area were previously copy-pasted at each call site (eight of them) and
// had already drifted once (see TECHDEBT.md's "Provider-drop reporting is
// inconsistent between merge and validate"). message and hint are passed
// through verbatim, so this does not paper over that drift — it only
// stops Severity/Area from being able to drift the same way.
func providerProblem(id, message, hint string) Problem {
	return Problem{
		Severity: SeverityWarn,
		Area:     AreaProvider,
		Subject:  id,
		Message:  message,
		Hint:     hint,
	}
}

// providerDropProblem is providerProblem for the common "provider X
// dropped: reason" message shape shared by every drop site in this
// package.
func providerDropProblem(id, reason, hint string) Problem {
	return providerProblem(id, fmt.Sprintf("provider %s dropped: %s", id, reason), hint)
}
