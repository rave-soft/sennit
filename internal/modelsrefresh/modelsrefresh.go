// Package modelsrefresh refreshes custom provider model-discovery caches.
// It is shared by `sennit models refresh` and the TUI's provider settings
// dialog.
package modelsrefresh

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/discover"
	"github.com/rave-soft/sennit/internal/modelcache"
)

// discoverTimeout bounds one provider's discovery and enrichment.
const discoverTimeout = 3 * time.Second

// ErrDiscoveryDisabled is a provider's Result.Err when its config sets
// discover_models: false. It is a failure, unlike the hand-written-models
// skip: the user asked for a refresh the config forbids.
var ErrDiscoveryDisabled = errors.New("discovery disabled (discover_models: false); define models in the config")

// DiscoverFn is the discovery seam used by RefreshWith.
type DiscoverFn func(context.Context, discover.Config, discover.Resolver) ([]catwalk.Model, error)

// Result is the outcome of refreshing one provider.
type Result struct {
	ID string
	// Models is the size of the refreshed list.
	Models         int
	Added, Removed int
	Skipped        bool
	SkipReason     string
	Err            error
}

// Refresh refreshes custom provider models using the standard discoverer.
func Refresh(ctx context.Context, cfg *config.ConfigStore, providerID string) ([]Result, error) {
	return RefreshWith(ctx, cfg, providerID, discover.DiscoverModels)
}

// RefreshWith refreshes one custom provider, or every enabled custom
// provider with a base_url when providerID is empty. It writes the
// global model-discovery cache and does not reload cfg: a caller that
// needs the new lists in memory calls cfg.ReloadFromDisk once afterwards.
func RefreshWith(ctx context.Context, cfg *config.ConfigStore, providerID string, discoverFn DiscoverFn) ([]Result, error) {
	if cfg == nil || cfg.Config() == nil {
		return nil, fmt.Errorf("configuration is unavailable")
	}
	catalog := make(map[string]struct{})
	for _, provider := range cfg.KnownProviders() {
		catalog[string(provider.ID)] = struct{}{}
	}
	if providerID != "" {
		if _, ok := catalog[providerID]; ok {
			return nil, fmt.Errorf("provider %q is a known catalog provider; refresh only applies to custom providers", providerID)
		}
		provider, ok := cfg.Config().Providers.Get(providerID)
		if !ok {
			return nil, fmt.Errorf("provider %q not found in config", providerID)
		}
		if provider.BaseURL == "" {
			return nil, fmt.Errorf("provider %q has no base_url configured", providerID)
		}
		return []Result{refreshOne(ctx, cfg, providerID, provider, discoverFn)}, nil
	}

	var ids []string
	for id, provider := range cfg.Config().Providers.Seq2() {
		if _, known := catalog[id]; !known && provider.BaseURL != "" && !provider.Disable {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	results := make([]Result, 0, len(ids))
	for _, id := range ids {
		provider, _ := cfg.Config().Providers.Get(id)
		results = append(results, refreshOne(ctx, cfg, id, provider, discoverFn))
	}
	return results, nil
}

func refreshOne(ctx context.Context, cfg *config.ConfigStore, id string, provider config.ProviderConfig, discoverFn DiscoverFn) Result {
	result := Result{ID: id}
	// discover_models: false is a hard stop, matching the guard
	// resolveDiscoveryRequests applies at load time (internal/providerload):
	// refresh must not second-guess an explicit opt-out.
	if provider.AutoDiscoverModels != nil && !*provider.AutoDiscoverModels {
		result.Err = ErrDiscoveryDisabled
		return result
	}
	// A hand-written models list must never be silently clobbered by a
	// refresh. discover_models: true is the explicit escape hatch: it
	// already means "always refresh, my models win on ID conflicts" at
	// load time, so it overrides this guard too.
	wantsDiscovery := provider.AutoDiscoverModels != nil && *provider.AutoDiscoverModels
	if provider.ModelsSource == config.ModelsSourceConfig && !wantsDiscovery {
		result.Skipped = true
		result.SkipReason = "models are explicitly defined in config"
		return result
	}
	if discoverFn == nil {
		result.Err = fmt.Errorf("model discoverer is unavailable")
		return result
	}
	discoverCtx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()
	dcfg := discover.Config{ID: id, BaseURL: provider.BaseURL, APIKey: provider.APIKey, ExtraHeaders: provider.ExtraHeaders, ProxyURL: provider.ProxyURL}
	models, err := discoverFn(discoverCtx, dcfg, cfg.Resolver())
	if err != nil {
		result.Err = err
		return result
	}
	if len(models) == 0 {
		result.Err = fmt.Errorf("no models returned")
		return result
	}
	if enricher := discover.GetEnricher(string(cmp.Or(provider.Type, catwalk.TypeOpenAICompat))); enricher != nil {
		models = enricher.EnrichModels(discoverCtx, dcfg, cfg.Resolver(), models)
	}
	result.Models = len(models)
	result.Added, result.Removed = DiffModelIDs(provider.Models, models)
	// Discovered models live in the global model-discovery cache, not
	// providers.<id>.models in the config; see
	// resolveCustomProviderModels in internal/providerload.
	path, err := cfg.ConfigPath(config.ScopeGlobal)
	if err != nil {
		result.Err = err
		return result
	}
	if err := modelcache.New(path).Save(id, models); err != nil {
		result.Err = err
	}
	return result
}

// DiffModelIDs compares a provider's current model list against a freshly
// discovered or fetched one and reports how many IDs are new and how many
// dropped out. It only compares by ID; a caller that also cares about
// per-model field changes (Codex's context-window updates, for instance)
// walks the fresh list itself and uses this just for the counts.
func DiffModelIDs(existing, fresh []catwalk.Model) (added, removed int) {
	existingIDs := make(map[string]bool, len(existing))
	for _, model := range existing {
		existingIDs[model.ID] = true
	}
	freshIDs := make(map[string]bool, len(fresh))
	for _, model := range fresh {
		if freshIDs[model.ID] {
			continue
		}
		freshIDs[model.ID] = true
		if !existingIDs[model.ID] {
			added++
		}
	}
	for id := range existingIDs {
		if !freshIDs[id] {
			removed++
		}
	}
	return added, removed
}
