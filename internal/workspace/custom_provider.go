package workspace

import (
	"context"
	"fmt"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/discover"
)

// ConfigureCustomProviderParams holds the user-supplied fields for a
// custom (non-catalog) provider.
type ConfigureCustomProviderParams struct {
	ID      string
	Name    string
	BaseURL string
	Type    string
	APIKey  string
}

// ConfigureCustomProvider persists a custom provider's configuration and
// runs model discovery against it, reusing the same discover.DiscoverModels
// / discover.GetEnricher core that `braid models refresh` uses (see
// internal/cmd/models.go's refreshCmd) so the two entry points share
// identical discovery behavior without duplicating it.
//
// It takes a [ConfigAccessor] rather than the full [Workspace] interface so
// it works transparently against both [AppWorkspace] and [ClientWorkspace]
// without any new RPC plumbing.
//
// Discovery runs first, against params directly, before anything is
// persisted. The result then decides how fields are ordered: every config
// mutator here (SetConfigField, SetProviderAPIKey) triggers a full config
// reload, and the loader itself auto-discovers models for any custom
// provider whose models list is still empty once base_url lands (see
// discoverCustomProviderModels in internal/config/load.go) — a provider
// that ends that reload with zero models is dropped from the in-memory
// catalog entirely. Writing our own already-discovered models before
// base_url avoids racing that loader-owned discovery and losing the
// provider we're trying to create; base_url is written last so the
// provider only "goes live" (in cfg.Providers) once every other field is
// already in place. Fields still land on disk even when discovery fails or
// returns nothing — callers should treat a zero-model result as "not yet
// usable" rather than deleted, since the user may fix the URL and retry via
// `braid models refresh <id>` or this same flow again.
func ConfigureCustomProvider(ctx context.Context, ws ConfigAccessor, scope config.Scope, params ConfigureCustomProviderParams) ([]catwalk.Model, error) {
	if params.ID == "" || params.BaseURL == "" {
		return nil, fmt.Errorf("provider ID and base URL are required")
	}

	dcfg := discover.Config{
		ID:      params.ID,
		BaseURL: params.BaseURL,
		APIKey:  params.APIKey,
	}
	models, discErr := discover.DiscoverModels(ctx, dcfg, ws.Resolver())
	if discErr == nil && len(models) > 0 {
		if enricher := discover.GetEnricher(params.Type); enricher != nil {
			if enriched, enrichErr := enricher.EnrichModels(ctx, dcfg, ws.Resolver(), models); enrichErr == nil {
				models = enriched
			}
		}
	}

	if err := ws.SetConfigField(scope, fmt.Sprintf("providers.%s.type", params.ID), params.Type); err != nil {
		return nil, fmt.Errorf("failed to save provider type: %w", err)
	}
	if params.Name != "" {
		if err := ws.SetConfigField(scope, fmt.Sprintf("providers.%s.name", params.ID), params.Name); err != nil {
			return nil, fmt.Errorf("failed to save provider name: %w", err)
		}
	}
	if len(models) > 0 {
		if err := ws.SetConfigField(scope, fmt.Sprintf("providers.%s.models", params.ID), models); err != nil {
			return nil, fmt.Errorf("failed to save discovered models: %w", err)
		}
	}
	if err := ws.SetConfigField(scope, fmt.Sprintf("providers.%s.base_url", params.ID), params.BaseURL); err != nil {
		return nil, fmt.Errorf("failed to save provider base URL: %w", err)
	}

	if params.APIKey != "" {
		// SetProviderAPIKey both writes the key and signals auth-complete,
		// matching the existing catalog-provider flow. It requires the
		// provider to already be known (catalog or already in
		// cfg.Providers); when discovery above failed, the base_url write
		// just above may have left the provider dropped from the
		// in-memory catalog (see the loader-race comment above), so fall
		// back to a raw field write to make sure the key isn't lost.
		if err := ws.SetProviderAPIKey(scope, params.ID, params.APIKey); err != nil {
			if fieldErr := ws.SetConfigField(scope, fmt.Sprintf("providers.%s.api_key", params.ID), params.APIKey); fieldErr != nil {
				return nil, fmt.Errorf("failed to save provider API key: %w", err)
			}
		}
	}

	if discErr != nil {
		return nil, discErr
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("no models found for provider %q — check the base URL and try again", params.ID)
	}

	return models, nil
}
