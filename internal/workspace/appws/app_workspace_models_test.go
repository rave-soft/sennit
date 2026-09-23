package appws

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/modelsrefresh"
	"github.com/stretchr/testify/require"
)

func TestAppWorkspaceRefreshProviderModelsUsesStoreAndProvider(t *testing.T) {
	original := refreshProviderModels
	t.Cleanup(func() { refreshProviderModels = original })

	store := configtest.NewStore(t, &config.Config{})
	var gotStore *config.ConfigStore
	var gotProvider string
	refreshProviderModels = func(_ context.Context, cfg *config.ConfigStore, providerID string) ([]modelsrefresh.Result, error) {
		gotStore = cfg
		gotProvider = providerID
		return []modelsrefresh.Result{{ID: providerID, Skipped: true, SkipReason: "discovery disabled"}}, nil
	}

	w := NewAppWorkspace(&app.App{}, store)
	results, err := w.RefreshProviderModels(t.Context(), "custom")
	require.NoError(t, err)
	require.Same(t, store, gotStore)
	require.Equal(t, "custom", gotProvider)
	require.Equal(t, "custom", results[0].ID)
	require.True(t, results[0].Skipped)
}

// TestAppWorkspaceRefreshProviderModelsReloadsAfterAWrite pins that a
// written cache is followed by a config reload. The store has no working
// directory, so the reload fails, and that error is what proves it ran.
func TestAppWorkspaceRefreshProviderModelsReloadsAfterAWrite(t *testing.T) {
	original := refreshProviderModels
	t.Cleanup(func() { refreshProviderModels = original })

	store := configtest.NewStore(t, &config.Config{})
	refreshProviderModels = func(_ context.Context, _ *config.ConfigStore, providerID string) ([]modelsrefresh.Result, error) {
		return []modelsrefresh.Result{{ID: providerID, Models: 2, Added: 2}}, nil
	}

	w := NewAppWorkspace(&app.App{}, store)
	results, err := w.RefreshProviderModels(t.Context(), "custom")
	require.ErrorContains(t, err, "cannot reload")
	require.Len(t, results, 1)
	require.Equal(t, 2, results[0].Models)
}
