package appws

import (
	"context"

	"github.com/rave-soft/sennit/internal/config"
	providerruntime "github.com/rave-soft/sennit/internal/providers/runtime"
	"github.com/rave-soft/sennit/internal/workspace"
)

// -- Config (read-only) --

// frontendConfigCacheEntry pairs a built *workspace.FrontendConfig with
// the *config.ConfigStore snapshot it was built from, so Config() can tell
// whether a cached DTO is still current without comparing its contents.
type frontendConfigCacheEntry struct {
	src *config.Config
	dto *workspace.FrontendConfig
}

// Config implements workspace.ConfigReader: the allowlist snapshot the UI
// (in-process today, remote once this is served over gRPC) is allowed to
// see. See workspace.FrontendConfig's doc comment for why this is not
// *config.Config.
//
// Cached on w.frontendConfigCache, keyed on the identity of the config
// pointer w.store.Config() returns. ConfigStore publishes an immutable
// snapshot per load/reload and swaps the pointer under its own lock
// (config.go's doc comments on Config/setConfig); a typed mutator
// (SetConfigField, RecordAccount, ...) goes through the same reload path,
// so a new config pointer is exactly the "something changed" signal this
// needs. w.store.KnownProviders() is reassigned in reloadFromDisk
// (internal/config/reload.go) in the same call that publishes the new
// config pointer - see NewFrontendConfig's callers here and in the DTO's
// own tests for why a fresh config pointer is a reliable proxy for "the
// catalog might have moved too", without this method having to compare
// the catalog slice on every call.
//
// The UI calls this several times per rendered frame (sidebar, header,
// model info, ...); with the embedded model catalog, rebuilding the DTO
// on every call is O(providers x models) allocation per call, so this
// caches the pointer rather than rebuilding it - see FrontendConfig's own
// doc comment for why the DTO is safe to share as long as nothing
// downstream mutates it. Config() is called from the Update goroutine and
// from tea.Cmd goroutines concurrently, so the cache is an atomic pointer,
// not a plain field: a racing rebuild after a reload just does the work
// twice, never a torn read.
func (w *AppWorkspace) Config() *workspace.FrontendConfig {
	src := w.store.Config()
	if cached := w.frontendConfigCache.Load(); cached != nil && cached.src == src {
		return cached.dto
	}
	dto := workspace.NewFrontendConfig(src, w.store.KnownProviders())
	w.frontendConfigCache.Store(&frontendConfigCacheEntry{src: src, dto: dto})
	return dto
}

// ServerConfig implements workspace.ServerConfigReader: the full,
// unredacted config, for callers that run only in-process (see that
// interface's doc comment for why this is not part of Workspace).
func (w *AppWorkspace) ServerConfig() *config.Config {
	return w.store.Config()
}

// ConfigStore returns the underlying [config.ConfigStore]. It exists so a
// caller building the UI's own preference store (internal/uiprefs) can wrap
// this same store, rather than reading display-only fields off Config() —
// see uiprefs's package doc. It is not part of the [workspace.Workspace]
// interface.
func (w *AppWorkspace) ConfigStore() *config.ConfigStore {
	return w.store
}

func (w *AppWorkspace) WorkingDir() string {
	return w.store.WorkingDir()
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

func (w *AppWorkspace) SetProviderAPIKey(scope config.Scope, providerID string, apiKey string) error {
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

func (w *AppWorkspace) ImportCopilot(ctx context.Context) (bool, error) {
	return w.app.Credentials().ImportCopilot(ctx)
}

func (w *AppWorkspace) RefreshOAuthToken(ctx context.Context, scope config.Scope, providerID string) error {
	return w.app.Credentials().RefreshOAuthToken(ctx, scope, providerID)
}

func (w *AppWorkspace) RefreshOAuthTokenForAccount(ctx context.Context, scope config.Scope, providerID, accountID string) error {
	return w.app.Credentials().RefreshOAuthTokenForAccount(ctx, scope, providerID, accountID)
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
