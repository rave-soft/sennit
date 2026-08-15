package config

import (
	"cmp"
	"fmt"
	"log/slog"
	"slices"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/braid/internal/discover"
)

// validateCustomProviders validates every provider outside the known
// catalog, applies any discovery results computed for it, and drops
// providers that end up unusable (unsupported type, disabled, no models, no
// endpoint). Providers whose models were freshly discovered are written to
// the global model-discovery cache (see saveCachedModels), so a later load
// can skip the HTTP round trip.
func (c *Config) validateCustomProviders(knownProviderNames map[string]bool, resolver VariableResolver, discoveryResults map[string]discoveryResult, globalDataPath string) error {
	for id, providerConfig := range c.Providers.Seq2() {
		if knownProviderNames[id] {
			continue
		}

		// Make sure the provider ID is set.
		providerConfig.ID = id
		providerConfig.Name = cmp.Or(providerConfig.Name, id) // Use ID as name if not set
		// Default to OpenAI if not set.
		providerConfig.Type = cmp.Or(providerConfig.Type, catwalk.TypeOpenAICompat)
		if !slices.Contains(catwalk.KnownProviderTypes(), providerConfig.Type) &&
			!discover.IsKnownCustomProvider(string(providerConfig.Type)) {
			slog.Warn("Skipping custom provider due to unsupported provider type", "provider", id)
			c.addProblem(Problem{
				Severity: SeverityWarn,
				Area:     AreaProvider,
				Subject:  id,
				Message:  fmt.Sprintf("provider %s dropped: unsupported type %q", id, providerConfig.Type),
			})
			c.Providers.Del(id)
			continue
		}

		if providerConfig.Disable {
			slog.Debug("Skipping custom provider due to disable flag", "provider", id)
			c.Providers.Del(id)
			continue
		}
		if providerConfig.BaseURL == "" {
			slog.Warn("Skipping custom provider due to missing API endpoint", "provider", id)
			c.addProblem(Problem{
				Severity: SeverityWarn,
				Area:     AreaProvider,
				Subject:  id,
				Message:  fmt.Sprintf("provider %s dropped: missing base_url", id),
			})
			c.Providers.Del(id)
			continue
		}

		// Apply discovery results if available.
		if result, ok := discoveryResults[id]; ok {
			if result.err != nil {
				slog.Warn("Model discovery failed", "provider", id, "error", result.err)
				if len(providerConfig.Models) == 0 {
					slog.Warn("Skipping provider with no models after failed discovery", "provider", id)
					c.Providers.Del(id)
					continue
				}
			} else if len(result.models) > 0 {
				providerConfig.Models = result.models
				providerConfig.ModelsSource = ModelsSourceCache
				slog.Info("Discovered models for provider", "provider", id, "count", len(result.models))
				// Persist to the model cache (not braid.json) so the next
				// load finds a non-empty models list via
				// resolveCustomProviderModels and skips this HTTP round
				// trip entirely. A failed discovery (the branch above)
				// must never reach here, so a down endpoint never touches
				// the cache.
				saveCachedModels(globalDataPath, id, result.models)
			}
		}

		if len(providerConfig.Models) == 0 {
			slog.Warn("Skipping custom provider because the provider has no models", "provider", id)
			c.addProblem(Problem{
				Severity: SeverityWarn,
				Area:     AreaProvider,
				Subject:  id,
				Message:  fmt.Sprintf("provider %s dropped: no models configured or discovered", id),
			})
			c.Providers.Del(id)
			continue
		}

		apiKey, err := resolver.ResolveValue(providerConfig.APIKey)
		if apiKey == "" || err != nil {
			slog.Warn("Provider is missing API key, this might be OK for local providers", "provider", id)
			c.addProblem(Problem{
				Severity: SeverityWarn,
				Area:     AreaProvider,
				Subject:  id,
				Message:  fmt.Sprintf("provider %s has no api_key", id),
				Hint:     "this is expected for local providers (Ollama, LM Studio, ...); ignore if intentional",
			})
		}
		baseURL, err := resolver.ResolveValue(providerConfig.BaseURL)
		if baseURL == "" || err != nil {
			slog.Warn("Skipping custom provider due to missing API endpoint", "provider", id, "error", err)
			c.addProblem(Problem{
				Severity: SeverityWarn,
				Area:     AreaProvider,
				Subject:  id,
				Message:  fmt.Sprintf("provider %s dropped: missing base_url", id),
			})
			c.Providers.Del(id)
			continue
		}

		// Custom-provider headers share the MCP error contract; see
		// mergeCatalogProviders' known-provider loop.
		for k, v := range providerConfig.ExtraHeaders {
			resolved, err := resolver.ResolveValue(v)
			if err != nil {
				return fmt.Errorf("resolving provider %s header %q: %w", id, k, err)
			}
			if resolved == "" {
				delete(providerConfig.ExtraHeaders, k)
				continue
			}
			providerConfig.ExtraHeaders[k] = resolved
		}

		// The proxy is optional and must never block provider loading the
		// way a missing api_key/base_url does, so a resolution failure
		// just clears the field and warns instead of skipping the provider.
		if providerConfig.ProxyURL != "" {
			resolvedProxy, err := resolver.ResolveValue(providerConfig.ProxyURL)
			if err != nil || resolvedProxy == "" {
				slog.Warn("Ignoring provider proxy_url due to resolution failure", "provider", id, "error", err)
				providerConfig.ProxyURL = ""
			} else {
				providerConfig.ProxyURL = resolvedProxy
			}
		}

		c.Providers.Set(id, providerConfig)
	}

	return nil
}
