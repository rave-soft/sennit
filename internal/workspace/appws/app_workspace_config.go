package appws

import (
	"context"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
)

// -- Config (read-only) --

func (w *AppWorkspace) Config() *config.Config {
	return w.store.Config()
}

func (w *AppWorkspace) WorkingDir() string {
	return w.store.WorkingDir()
}

func (w *AppWorkspace) Resolver() config.VariableResolver {
	return w.store.Resolver()
}

// -- Config mutations --

func (w *AppWorkspace) UpdatePreferredModel(scope config.Scope, model config.SelectedModel) error {
	return w.store.UpdatePreferredModel(scope, model)
}

// OverridePreferredModel sets the model in memory only, without
// touching the user's config file. See the Workspace interface doc.
func (w *AppWorkspace) OverridePreferredModel(model config.SelectedModel) error {
	w.store.OverridePreferredModel(model)
	return nil
}

func (w *AppWorkspace) SetCompactMode(scope config.Scope, enabled bool) error {
	return w.store.SetCompactMode(scope, enabled)
}

func (w *AppWorkspace) SetProviderAPIKey(scope config.Scope, providerID string, apiKey any) error {
	if err := w.store.SetProviderAPIKey(scope, providerID, apiKey); err != nil {
		return err
	}
	w.app.Credentials().SignalAuthComplete(providerID)
	return nil
}

func (w *AppWorkspace) SetConfigField(scope config.Scope, key string, value any) error {
	return w.store.SetConfigField(scope, key, value)
}

func (w *AppWorkspace) RemoveConfigField(scope config.Scope, key string) error {
	return w.store.RemoveConfigField(scope, key)
}

func (w *AppWorkspace) ImportCopilot() (*oauth.Token, bool) {
	return w.app.Credentials().ImportCopilot()
}

func (w *AppWorkspace) RefreshOAuthToken(ctx context.Context, scope config.Scope, providerID string) error {
	return w.app.Credentials().RefreshOAuthToken(ctx, scope, providerID)
}
