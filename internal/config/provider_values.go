package config

import (
	"fmt"
	"log/slog"
	"maps"
)

func ResolveProviderHeaders(headers map[string]string, resolver VariableResolver, providerID string) error {
	resolved, err := resolveMap(headers, resolver, func(key string) string {
		return fmt.Sprintf("resolving provider %s header %q", providerID, key)
	}, true)
	if err != nil {
		return err
	}
	clear(headers)
	maps.Copy(headers, resolved)
	return nil
}

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
