package appws

import (
	"context"
	"errors"

	"github.com/rave-soft/sennit/internal/modelsrefresh"
	"github.com/rave-soft/sennit/internal/wireerr"
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
			Updated: len(result.ContextWindowChanges),
			Skipped: result.Skipped, SkipReason: result.SkipReason,
			Err: encodeRefreshErr(result.Err),
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

// encodeRefreshErr is workspace.EncodeError plus one special case:
// modelsrefresh.ErrDiscoveryDisabled has no counterpart workspace.EncodeError
// can recognize on its own, since internal/workspace must not import
// internal/modelsrefresh (see workspace.ErrDiscoveryDisabled's doc comment
// for why). This package already imports modelsrefresh, so it is the
// right place to translate that one sentinel into the "discovery_disabled"
// code before falling back to the generic encoder.
func encodeRefreshErr(err error) *wireerr.Error {
	if errors.Is(err, modelsrefresh.ErrDiscoveryDisabled) {
		return &wireerr.Error{Code: "discovery_disabled", Message: err.Error()}
	}
	return workspace.EncodeError(err)
}
