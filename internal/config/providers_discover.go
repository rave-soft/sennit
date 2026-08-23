package config

import (
	"cmp"
	"context"
	"fmt"
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
//     ModelsSourceCache. This lets a restart skip runDiscoveryRequests'
//     HTTP round trip for an empty models list — the cache, not
//     providers.<id>.models in sennit.json, is where auto-discovered
//     models live now. A provider with discover_models: true explicitly
//     still refreshes over HTTP regardless (resolveDiscoveryRequests'
//     wantsDiscovery branch doesn't check Models length).
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

// discoveryRequest is one custom provider's fully-resolved discovery input,
// ready to run over HTTP without consulting the resolver (and, through it,
// process env) again. See resolveDiscoveryRequests.
type discoveryRequest struct {
	id           string
	providerType catwalk.Type
	cfg          discover.Config
}

// resolveDiscoveryRequests decides which custom providers need discovery
// (discover_models explicitly true, or an empty Models list unless opted
// out) and resolves every value the HTTP round trip will need — base URL,
// API key, and extra headers — via resolver.
//
// This must run while the caller still holds processEnvMu (see
// PushPopEnvOverrides): resolver.ResolveValue reads process env on every
// call, so the SENNIT_ overrides have to be in place here. The actual HTTP
// call does not need them — see runDiscoveryRequests — which is the whole
// point of splitting this out: it lets configureProviders drop the lock
// before the slow part runs.
//
// A base_url/api_key resolution failure is reported as a discoveryResult
// here, mirroring the error discover.DiscoverModels used to produce from
// the same failure inside doRequest, so validateCustomProviders sees the
// identical outcome either way. A header resolution failure is not fatal,
// matching doRequest: that header is just dropped.
func resolveDiscoveryRequests(providers *csync.Map[string, ProviderConfig], knownProviderNames map[string]bool, resolver VariableResolver) ([]discoveryRequest, map[string]discoveryResult) {
	var requests []discoveryRequest
	results := make(map[string]discoveryResult)

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

		baseURL, err := resolver.ResolveValue(pc.BaseURL)
		if err != nil {
			results[id] = discoveryResult{err: fmt.Errorf("discover models for provider %s: resolve base_url: %w", providerID, err)}
			continue
		}
		apiKey, err := resolver.ResolveValue(pc.APIKey)
		if err != nil {
			results[id] = discoveryResult{err: fmt.Errorf("discover models for provider %s: resolve api_key: %w", providerID, err)}
			continue
		}
		headers := make(map[string]string, len(pc.ExtraHeaders))
		for k, v := range pc.ExtraHeaders {
			resolved, err := resolver.ResolveValue(v)
			if err != nil || resolved == "" {
				continue
			}
			headers[k] = resolved
		}

		requests = append(requests, discoveryRequest{
			id:           id,
			providerType: cmp.Or(pc.Type, catwalk.TypeOpenAICompat),
			cfg: discover.Config{
				ID:             providerID,
				BaseURL:        baseURL,
				APIKey:         apiKey,
				ExtraHeaders:   headers,
				ExistingModels: pc.Models,
				// ProxyURL is carried through unresolved, matching prior
				// behavior: discover.Config's httpClient() has never run it
				// through a resolver, only url.Parse.
				ProxyURL: pc.ProxyURL,
			},
		})
	}

	return requests, results
}

// runDiscoveryRequests runs the HTTP round trip for every pre-resolved
// discovery request concurrently. Every value it sends was already resolved
// by resolveDiscoveryRequests, so this never calls back into the resolver —
// it does not touch process env and does not need processEnvMu held. It
// also does not touch the store, a config pointer, or writeMu, so callers
// are expected to run it without holding writeMu so a slow endpoint
// (bounded by the 3s timeout below) never blocks unrelated config mutators.
func runDiscoveryRequests(ctx context.Context, requests []discoveryRequest) map[string]discoveryResult {
	discoveryResults := make(map[string]discoveryResult, len(requests))
	var mu sync.Mutex
	var wg sync.WaitGroup

	discoverCtx, discoverCancel := context.WithTimeout(ctx, 3*time.Second)
	defer discoverCancel()
	for _, req := range requests {
		wg.Go(func() {
			models, err := discover.DiscoverModels(discoverCtx, req.cfg, IdentityResolver())
			if err == nil && len(models) > 0 {
				if enricher := discover.GetEnricher(string(req.providerType)); enricher != nil {
					models = enricher.EnrichModels(discoverCtx, req.cfg, IdentityResolver(), models)
				}
			}
			mu.Lock()
			discoveryResults[req.id] = discoveryResult{models: models, err: err}
			mu.Unlock()
		})
	}
	wg.Wait()

	return discoveryResults
}
