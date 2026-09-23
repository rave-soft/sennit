package appws

import (
	"context"

	"github.com/rave-soft/sennit/internal/modelsrefresh"
	"github.com/rave-soft/sennit/internal/workspace"
)

// refreshProviderModels is modelsrefresh.Refresh, a variable so tests can
// substitute it without reaching a provider.
var refreshProviderModels = modelsrefresh.Refresh

// RefreshProviderModels implements Workspace. When at least one provider's
// cache was written, it reloads the config once, so the new lists replace
// the in-memory ones, and rebuilds the agent models.
func (w *AppWorkspace) RefreshProviderModels(ctx context.Context, providerID string) ([]workspace.ModelRefreshResult, error) {
	results, err := refreshProviderModels(ctx, w.store, providerID)
	if err != nil {
		return nil, err
	}

	converted := make([]workspace.ModelRefreshResult, len(results))
	refreshed := false
	for i, result := range results {
		converted[i] = workspace.ModelRefreshResult{
			ID: result.ID, Models: result.Models,
			Added: result.Added, Removed: result.Removed,
			Skipped: result.Skipped, SkipReason: result.SkipReason,
			Err: result.Err,
		}
		refreshed = refreshed || (!result.Skipped && result.Err == nil)
	}
	if !refreshed {
		return converted, nil
	}
	if err := w.store.ReloadFromDisk(ctx); err != nil {
		return converted, err
	}
	if err := w.app.UpdateAgentModel(ctx); err != nil {
		return converted, err
	}
	return converted, nil
}
