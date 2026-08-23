package config

import (
	"cmp"
	"context"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/discover"
)

// resolveCustomProviderModels does two things for every custom provider,
// before discovery runs:
//
//  1. Marks ModelsSource — ModelsSourceConfig for a provider that already
//     has a non-empty, hand-written Models list, so later code (`sennit
//     models refresh`'s explicit-config guard, in internal/cmd/models.go)
//     can tell "the user wrote these" apart from "these came from
//     discovery" without re-deriving it. This must run before the cache
//     fill below, since that fill only ever touches providers with an
//     empty list.
//  2. Fills in Models for a provider whose config has none, from the
//     global model-discovery cache (see saveCachedModels), marking it
//     ModelsSourceCache. This lets a restart skip
//     discoverCustomProviderModels' HTTP round trip for an empty models
//     list — the cache, not providers.<id>.models in sennit.json, is where
//     auto-discovered models live now. A provider with discover_models:
//     true explicitly still refreshes over HTTP regardless
//     (discoverCustomProviderModels's wantsDiscovery branch doesn't check
//     Models length).
func resolveCustomProviderModels(providers *csync.Map[string, ProviderConfig], knownProviderNames map[string]bool, globalDataPath string) {
	for id, pc := range providers.Seq2() {
		if knownProviderNames[id] {
			continue
		}
		if len(pc.Models) > 0 {
			pc.ModelsSource = ModelsSourceConfig
			providers.Set(id, pc)
			continue
		}
		if pc.Disable || pc.BaseURL == "" {
			continue
		}
		if models, ok := loadCachedModels(globalDataPath, id); ok {
			pc.Models = models
			pc.ModelsSource = ModelsSourceCache
			providers.Set(id, pc)
		}
	}
}

// discoveryResult holds the outcome of a single custom provider's
// model-discovery HTTP call.
type discoveryResult struct {
	models []catwalk.Model
	err    error
}

// discoverCustomProviderModels runs model discovery concurrently for custom
// providers that need it. A provider needs discovery when discover_models is
// explicitly true, or when its models list is empty (auto-trigger, unless
// opted out).
//
// This is pure computation plus HTTP calls against providers — it does not
// touch the store, a config pointer, or any lock. Callers are expected to
// run it without holding writeMu so a slow endpoint (bounded by the 3s
// timeout below) never blocks unrelated config mutators.
func discoverCustomProviderModels(ctx context.Context, providers *csync.Map[string, ProviderConfig], knownProviderNames map[string]bool, resolver VariableResolver) map[string]discoveryResult {
	discoveryResults := make(map[string]discoveryResult)
	var mu sync.Mutex
	var wg sync.WaitGroup

	discoverCtx, discoverCancel := context.WithTimeout(ctx, 3*time.Second)
	defer discoverCancel()
	for id, pc := range providers.Seq2() {
		if knownProviderNames[id] {
			continue
		}
		if pc.Disable || pc.BaseURL == "" {
			continue
		}
		if pc.AutoDiscoverModels != nil && !*pc.AutoDiscoverModels {
			// Explicit discover_models: false is a hard stop — never
			// discover, even with an empty Models list (e.g. a broken or
			// path-echoing /models endpoint the user is deliberately
			// working around by hand; see junkModelID in
			// internal/discover).
			continue
		}
		wantsDiscovery := pc.AutoDiscoverModels != nil && *pc.AutoDiscoverModels
		autoTrigger := len(pc.Models) == 0 && (pc.AutoDiscoverModels == nil || *pc.AutoDiscoverModels)
		if !wantsDiscovery && !autoTrigger {
			continue
		}
		providerID := cmp.Or(pc.ID, id)
		cfg := discover.Config{
			ID:             providerID,
			BaseURL:        pc.BaseURL,
			APIKey:         pc.APIKey,
			ExtraHeaders:   pc.ExtraHeaders,
			ExistingModels: pc.Models,
			ProxyURL:       pc.ProxyURL,
		}
		providerType := cmp.Or(pc.Type, catwalk.TypeOpenAICompat)
		wg.Go(func() {
			models, err := discover.DiscoverModels(discoverCtx, cfg, resolver)
			if err == nil && len(models) > 0 {
				if enricher := discover.GetEnricher(string(providerType)); enricher != nil {
					models = enricher.EnrichModels(discoverCtx, cfg, resolver, models)
				}
			}
			mu.Lock()
			discoveryResults[id] = discoveryResult{models: models, err: err}
			mu.Unlock()
		})
	}
	wg.Wait()

	return discoveryResults
}
