package providerconfig

import (
	"fmt"
	"log/slog"
	"maps"
)

// ResolveProviderHeaders resolves every value in headers in place through
// resolver, dropping any header whose resolved value is empty.
func ResolveProviderHeaders(headers map[string]string, resolver VariableResolver, providerID string) error {
	resolved, err := ResolveMap(headers, resolver, func(key string) string {
		return fmt.Sprintf("resolving provider %s header %q", providerID, key)
	}, true)
	if err != nil {
		return err
	}
	clear(headers)
	maps.Copy(headers, resolved)
	return nil
}

// ResolveOptionalProviderProxy resolves proxyURL through resolver,
// returning "" (and logging) on any resolution failure rather than
// propagating the error — a bad proxy_url falls back to no per-provider
// override instead of failing provider setup outright.
func ResolveOptionalProviderProxy(proxyURL string, resolver VariableResolver, providerID string) string {
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
