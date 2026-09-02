package config

import providerconfig "github.com/rave-soft/sennit/internal/providers/config"

// ResolveProviderHeaders and ResolveOptionalProviderProxy live in
// internal/providers/config now — internal/providers/runtime calls them
// directly, and internal/config needs to call into
// internal/providers/runtime, so they cannot live in internal/config
// without recreating an import cycle. Kept as thin forwarders here so
// existing config.ResolveProviderHeaders / config.ResolveOptionalProviderProxy
// call sites across the tree do not need to change.
func ResolveProviderHeaders(headers map[string]string, resolver VariableResolver, providerID string) error {
	return providerconfig.ResolveProviderHeaders(headers, resolver, providerID)
}

func ResolveOptionalProviderProxy(proxyURL string, resolver VariableResolver, providerID string) string {
	return providerconfig.ResolveOptionalProviderProxy(proxyURL, resolver, providerID)
}
