package appws

import (
	"context"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/discover"
	"github.com/rave-soft/sennit/internal/workspace"
)

// ConfigureCustomProvider implements workspace.CustomProviderConfigurer,
// supplying discoverCustomProviderModels as the live discovery step that
// internal/workspace itself must not import — see
// workspace.ModelDiscoverer's doc comment.
func (w *AppWorkspace) ConfigureCustomProvider(ctx context.Context, scope config.Scope, params workspace.ConfigureCustomProviderParams) ([]catwalk.Model, error) {
	return workspace.ConfigureCustomProviderUsing(ctx, w, scope, params, discoverCustomProviderModels)
}

// discoverCustomProviderModels runs the same discover.DiscoverModels /
// discover.GetEnricher core `sennit models refresh` uses (see
// internal/cmd/models.go's refreshCmd) so the two entry points share
// identical discovery behavior without duplicating it.
func discoverCustomProviderModels(ctx context.Context, params workspace.ConfigureCustomProviderParams, resolver config.VariableResolver) ([]catwalk.Model, error) {
	dcfg := discover.Config{
		ID:      params.ID,
		BaseURL: params.BaseURL,
		APIKey:  params.APIKey,
	}
	models, err := discover.DiscoverModels(ctx, dcfg, resolver)
	if err != nil || len(models) == 0 {
		return models, err
	}
	if enricher := discover.GetEnricher(params.Type); enricher != nil {
		models = enricher.EnrichModels(ctx, dcfg, resolver, models)
	}
	return models, nil
}
