package providerload

import (
	"cmp"
	"fmt"
	"log/slog"
	"slices"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/modelcache"
	"github.com/rave-soft/sennit/internal/discover"
)

// Hints shared by every drop of the same class in this file, so a custom
// provider dropped for a reason catalog providers also get dropped for
// (missing base_url/endpoint) reads the same guidance either way. See
// providerDropProblem's callers in providers_merge.go for the catalog side.
const (
	hintMissingBaseURL = "static check only; set base_url and reload"
	hintNoModels       = "static check only; configure at least one model and reload"
)

// validateCustomProviders validates every provider outside the known
// catalog, applies any discovery results computed for it, and drops
// providers that end up unusable (unsupported type, disabled, no models, no
// endpoint). Providers whose models were freshly discovered are written to
// the global model-discovery cache, so a later load
// can skip the HTTP round trip.
func (l *Loader) validateCustomProviders(cfg *config.Config, knownProviderNames map[string]bool, resolver config.VariableResolver, discoveryResults map[string]discoveryResult, globalDataPath string) error {
	for id, providerConfig := range cfg.Providers.Seq2() {
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
			l.dropProvider(cfg, id, problem.Message, problem.Hint)
			continue
		}

		if providerConfig.Disable {
			// Deliberately quiet: the user asked for this provider to be
			// off, so unlike the drops below, this is not something
			// `sennit doctor` should flag as a problem.
			cfg.Providers.Del(id)
			continue
		}
		if providerConfig.BaseURL == "" {
			problem := providerDropProblem(id, "missing base_url", hintMissingBaseURL)
			l.dropProvider(cfg, id, problem.Message, problem.Hint)
			continue
		}

		// Apply discovery results if available.
		if result, ok := discoveryResults[id]; ok {
			if result.err != nil {
				slog.Warn("Model discovery failed", "provider", id, "error", result.err)
				if len(providerConfig.Models) == 0 {
					// Same underlying condition as the "no models configured
					// or discovered" drop below, so it gets the same Problem
					// treatment and Hint — previously this branch dropped
					// silently while the other recorded a Problem.
					problem := providerDropProblem(id, "no models after failed discovery", hintNoModels)
					l.dropProvider(cfg, id, problem.Message, problem.Hint)
					continue
				}
			} else if len(result.models) > 0 {
				// DiscoverModels always starts its result from
				// cfg.ExistingModels (see internal/discover), so a
				// provider that already had hand-written models (only
				// possible here via an explicit discover_models: true,
				// since autoTrigger requires an empty list) gets those
				// same models back merged with anything freshly
				// discovered. hadConfigModels tells the two cases apart
				// below; freshlyDiscovered strips the echoed-back
				// pre-discovery models (by ID) so only genuinely new
				// entries are ever written to the cache — otherwise a
				// hand-written model would be persisted there and
				// resurface after being deleted from config.
				hadConfigModels := len(providerConfig.Models) > 0
				freshlyDiscovered := newModelsOnly(providerConfig.Models, result.models)
				providerConfig.Models = result.models
				slog.Info("Discovered models for provider", "provider", id, "count", len(result.models))
				if hadConfigModels {
					// Keep the Config attribution instead of flipping the
					// whole (now merged) list to Cache: the provider's
					// identity as "has hand-written models" doesn't change
					// just because discover_models: true also ran
					// discovery.
					providerConfig.ModelsSource = config.ModelsSourceConfig
				} else {
					providerConfig.ModelsSource = config.ModelsSourceCache
				}
				if len(freshlyDiscovered) > 0 {
					// Persist only the freshly discovered models to the
					// model cache (not sennit.json), so the next load finds
					// them via resolveCustomProviderModels and skips this
					// HTTP round trip entirely, without also caching (and
					// later resurrecting) any hand-written models this
					// provider already had. A failed discovery (the branch
					// above) must never reach here, so a down endpoint
					// never touches the cache.
					modelcache.New(globalDataPath).SaveBestEffort(id, freshlyDiscovered)
				}
			}
		}

		if len(providerConfig.Models) == 0 {
			problem := providerDropProblem(id, "no models configured or discovered", hintNoModels)
			l.dropProvider(cfg, id, problem.Message, problem.Hint)
			continue
		}

		apiKey, err := resolver.ResolveValue(providerConfig.APIKey)
		if apiKey == "" || err != nil {
			slog.Warn("Provider is missing API key, this might be OK for local providers", "provider", id)
			cfg.AddRuntimeProblem(providerProblem(id, fmt.Sprintf("provider %s has no api_key", id),
				"this is expected for local providers (Ollama, LM Studio, ...); ignore if intentional"))
		}
		baseURL, err := resolver.ResolveValue(providerConfig.BaseURL)
		if baseURL == "" || err != nil {
			problem := providerDropProblem(id, "missing base_url", hintMissingBaseURL)
			l.dropProvider(cfg, id, problem.Message, problem.Hint)
			continue
		}

		// Custom-provider headers share the MCP error contract; see
		// mergeCatalogProviders' known-provider loop.
		if err := resolveProviderHeaders(providerConfig.ExtraHeaders, resolver, id); err != nil {
			return err
		}

		providerConfig.ProxyURL = resolveOptionalProxy(providerConfig.ProxyURL, resolver, id)
		providerConfig.ConfiguredProxyURL = providerConfig.ProxyURL

		cfg.Providers.Set(id, providerConfig)
	}

	return nil
}

// newModelsOnly returns the entries in merged whose ID is not present in
// preDiscovery. DiscoverModels' result always starts from cfg.ExistingModels
// (== preDiscovery here) before appending anything actually fetched over
// HTTP, so this recovers just the genuinely new models — the ones safe to
// write to the discovery cache. See the caller in validateCustomProviders.
func newModelsOnly(preDiscovery, merged []catwalk.Model) []catwalk.Model {
	if len(preDiscovery) == 0 {
		return merged
	}
	existing := make(map[string]struct{}, len(preDiscovery))
	for _, m := range preDiscovery {
		existing[m.ID] = struct{}{}
	}
	var fresh []catwalk.Model
	for _, m := range merged {
		if _, ok := existing[m.ID]; ok {
			continue
		}
		fresh = append(fresh, m)
	}
	return fresh
}
