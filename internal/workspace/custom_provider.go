package workspace

import (
	"context"
	"fmt"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
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

// CustomProviderConfigurer persists a custom provider's configuration and
// discovers its models. It is a one-method role interface — like
// ProviderAPIKeySetter and ConfigureCustomProvider's other single-method
// siblings documented on [FrontendWorkspace] — rather than a wider
// [Workspace] method taking an injected discovery callback:
// internal/ui/model/dialog_actions.go wants exactly this capability, named,
// and nothing else. The [Workspace] contract itself still must not import
// internal/discover, so the live HTTP call lives in the implementation
// (internal/workspace/appws), driven through [ModelDiscoverer] below.
type CustomProviderConfigurer interface {
	ConfigureCustomProvider(ctx context.Context, scope config.Scope, params ConfigureCustomProviderParams) ([]catwalk.Model, error)
}

// ModelDiscoverer is the one capability [ConfigureCustomProviderUsing]
// needs beyond writing config: discovering params' models over HTTP. This
// contract package must not import internal/discover — that would put a
// live network client on every consumer of [Workspace], including a
// read-only thread view — so the discovery itself is supplied by the
// caller. internal/workspace/appws is the only implementation today,
// wrapping internal/discover's model-discovery and enrichment calls, the
// same core `sennit models refresh` uses (see internal/cmd/models.go's
// refreshCmd).
type ModelDiscoverer func(ctx context.Context, params ConfigureCustomProviderParams, resolver config.VariableResolver) ([]catwalk.Model, error)

// customProviderWriter is what ConfigureCustomProviderUsing needs to
// persist a provider's configuration, once discovery has already run.
type customProviderWriter interface {
	ConfigResolver
	ConfigFieldEditor
	ProviderAPIKeySetter
}

// ConfigureCustomProviderUsing persists a custom provider's configuration
// and runs model discovery against it via discoverModels.
//
// It takes only the resolver and config-writing capabilities it needs
// rather than the full [Workspace] interface, so it works against any
// implementation without depending on unrelated workspace operations.
//
// Discovery runs first, against params directly, before anything is
// persisted. The result then decides how fields are ordered: the config
// mutators here reload (SetConfigField always; SetProviderAPIKey only when
// it introduces a provider the config did not have — see
// reloadNewProvider), and the loader itself auto-discovers models for any custom
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
// `sennit models refresh <id>` or this same flow again.
func ConfigureCustomProviderUsing(ctx context.Context, ws customProviderWriter, scope config.Scope, params ConfigureCustomProviderParams, discoverModels ModelDiscoverer) ([]catwalk.Model, error) {
	if params.ID == "" || params.BaseURL == "" {
		return nil, fmt.Errorf("provider ID and base URL are required")
	}

	models, discErr := discoverModels(ctx, params, ws.Resolver())

	if err := ws.SetConfigField(scope, config.ProviderFieldKey(params.ID, "type"), params.Type); err != nil {
		return nil, fmt.Errorf("failed to save provider type: %w", err)
	}
	if params.Name != "" {
		if err := ws.SetConfigField(scope, config.ProviderFieldKey(params.ID, "name"), params.Name); err != nil {
			return nil, fmt.Errorf("failed to save provider name: %w", err)
		}
	}
	if len(models) > 0 {
		if err := ws.SetConfigField(scope, config.ProviderFieldKey(params.ID, "models"), models); err != nil {
			return nil, fmt.Errorf("failed to save discovered models: %w", err)
		}
	}
	if err := ws.SetConfigField(scope, config.ProviderFieldKey(params.ID, "base_url"), params.BaseURL); err != nil {
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
			if fieldErr := ws.SetConfigField(scope, config.ProviderFieldKey(params.ID, "api_key"), params.APIKey); fieldErr != nil {
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
