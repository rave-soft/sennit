package appws

import (
	"context"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	providerruntime "github.com/rave-soft/sennit/internal/providers/runtime"
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

// VerifyProviderAPIKey tests apiKey against providerID by building the same
// kind of runtime provider the agent itself would use — starting from the
// provider's already-configured entry (proxy, extra headers, rotation) when
// one exists, or the known-providers catalog entry (base URL, type, name)
// for a provider not yet configured — and swapping in apiKey, then probing
// it with providers/runtime.TestConnection. This is deliberately not just
// "build whatever the caller typed": a hand-assembled ProviderConfig tests
// a different provider than the one the agent would end up talking to.
func (w *AppWorkspace) VerifyProviderAPIKey(ctx context.Context, providerID, apiKey string) error {
	cfg := w.store.Config()
	pc, exists := cfg.Providers.Get(providerID)
	if !exists {
		pc = config.ProviderConfig{ID: providerID}
		for _, known := range w.store.KnownProviders() {
			if string(known.ID) == providerID {
				pc.Name = known.Name
				pc.BaseURL = known.APIEndpoint
				pc.Type = known.Type
				break
			}
		}
	}
	pc.APIKey = apiKey

	resolver := w.store.Resolver()
	provider, err := providerruntime.FromConfig(pc, resolver)
	if err != nil {
		return err
	}
	return providerruntime.TestConnection(ctx, provider, resolver)
}
