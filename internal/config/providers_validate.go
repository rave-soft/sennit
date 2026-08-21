package config

import (
	"cmp"
	"fmt"
	"log/slog"
	"slices"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/discover"
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
			problem := providerDropProblem(id, fmt.Sprintf("unsupported type %q", providerConfig.Type), "")
			c.dropProvider(id, slog.Warn, "Skipping custom provider due to unsupported provider type", []any{"provider", id}, &problem)
			continue
		}

		if providerConfig.Disable {
			// Deliberately quiet: the user asked for this provider to be
			// off, so unlike the drops below, this is not something
			// `sennit doctor` should flag as a problem.
			c.dropProvider(id, slog.Debug, "Skipping custom provider due to disable flag", []any{"provider", id}, nil)
			continue
		}
		if providerConfig.BaseURL == "" {
			problem := providerDropProblem(id, "missing base_url", "")
			c.dropProvider(id, slog.Warn, "Skipping custom provider due to missing API endpoint", []any{"provider", id}, &problem)
			continue
		}

		// Apply discovery results if available.
		if result, ok := discoveryResults[id]; ok {
			if result.err != nil {
				slog.Warn("Model discovery failed", "provider", id, "error", result.err)
				if len(providerConfig.Models) == 0 {
					// No Problem recorded here, unlike the other drops in
					// this function: this looks like accidental drift
					// rather than a deliberate choice (see the "no models"
					// drop a few lines down, which does record one for the
					// same underlying condition), preserved as-is.
					c.dropProvider(id, slog.Warn, "Skipping provider with no models after failed discovery", []any{"provider", id}, nil)
					continue
				}
			} else if len(result.models) > 0 {
				providerConfig.Models = result.models
				providerConfig.ModelsSource = ModelsSourceCache
				slog.Info("Discovered models for provider", "provider", id, "count", len(result.models))
				// Persist to the model cache (not sennit.json) so the next
				// load finds a non-empty models list via
				// resolveCustomProviderModels and skips this HTTP round
				// trip entirely. A failed discovery (the branch above)
				// must never reach here, so a down endpoint never touches
				// the cache.
				saveCachedModels(globalDataPath, id, result.models)
			}
		}

		if len(providerConfig.Models) == 0 {
			problem := providerDropProblem(id, "no models configured or discovered", "")
			c.dropProvider(id, slog.Warn, "Skipping custom provider because the provider has no models", []any{"provider", id}, &problem)
			continue
		}

		apiKey, err := resolver.ResolveValue(providerConfig.APIKey)
		if apiKey == "" || err != nil {
			slog.Warn("Provider is missing API key, this might be OK for local providers", "provider", id)
			c.addProblem(providerProblem(id, fmt.Sprintf("provider %s has no api_key", id),
				"this is expected for local providers (Ollama, LM Studio, ...); ignore if intentional"))
		}
		baseURL, err := resolver.ResolveValue(providerConfig.BaseURL)
		if baseURL == "" || err != nil {
			problem := providerDropProblem(id, "missing base_url", "")
			c.dropProvider(id, slog.Warn, "Skipping custom provider due to missing API endpoint", []any{"provider", id, "error", err}, &problem)
			continue
		}

		// Custom-provider headers share the MCP error contract; see
		// mergeCatalogProviders' known-provider loop.
		if err := resolveProviderHeaders(providerConfig.ExtraHeaders, resolver, id); err != nil {
			return err
		}

		providerConfig.ProxyURL = resolveOptionalProxy(providerConfig.ProxyURL, resolver, id)

		c.Providers.Set(id, providerConfig)
	}

	return nil
}
