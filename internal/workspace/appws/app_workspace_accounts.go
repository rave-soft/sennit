package appws

import (
	"context"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/discover"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/accounts"
)

// -- Accounts --

// accountStore opens the on-disk provider-accounts file store. It is opened
// fresh on every call rather than cached on AppWorkspace: FileStore is a
// thin, stateless wrapper over the file path (see its doc comment — it
// holds no in-memory cache), so there is nothing worth keeping alive
// between calls.
func (w *AppWorkspace) accountStore() *accounts.FileStore {
	return accounts.NewFileStore(config.GlobalAccountsFile())
}

func (w *AppWorkspace) accounts() *config.AccountsService {
	return config.NewAccountsService(w.store, w.accountStore(), fetchCodexUsage)
}

// RecordAccount implements Workspace.
func (w *AppWorkspace) RecordAccount(scope config.Scope, providerID string, cred accounts.LegacyCredential) (accounts.Account, error) {
	a, err := w.accounts().Record(scope, providerID, cred)
	if err != nil {
		return accounts.Account{}, err
	}
	w.app.Credentials().SignalAuthComplete(providerID)
	return a, nil
}

// ListAccounts implements Workspace.
func (w *AppWorkspace) ListAccounts(providerID string) ([]accounts.Account, error) {
	return w.accounts().List(providerID)
}

// ActivateAccount implements Workspace.
func (w *AppWorkspace) ActivateAccount(scope config.Scope, providerID, accountID string) error {
	return w.accounts().Activate(scope, providerID, accountID)
}

// UpdateAccount implements Workspace.
func (w *AppWorkspace) UpdateAccount(providerID string, account accounts.Account) error {
	return w.accounts().Update(providerID, account)
}

// RemoveAccount implements Workspace.
func (w *AppWorkspace) RemoveAccount(scope config.Scope, providerID, accountID string) error {
	return w.accounts().Remove(scope, providerID, accountID)
}

// PurgeAccounts implements Workspace.
func (w *AppWorkspace) PurgeAccounts(scope config.Scope, providerID string) error {
	return w.accounts().Purge(scope, providerID)
}

// SetProviderProxy implements Workspace.
func (w *AppWorkspace) SetProviderProxy(providerID, proxy string) error {
	return w.accounts().SetProviderProxy(providerID, proxy)
}

// RefreshAccountLimits implements Workspace.
func (w *AppWorkspace) RefreshAccountLimits(ctx context.Context, providerID string) ([]accounts.Account, error) {
	return w.accounts().RefreshLimits(ctx, providerID)
}

// fetchCodexUsage adapts codex.FetchUsage to config.AccountUsageFetcher by
// converting the vendor's own shape into the stored snapshot.
func fetchCodexUsage(ctx context.Context, proxyURL, accessToken, accountID string) (accounts.Usage, bool, error) {
	u, ok, err := codex.FetchUsage(ctx, proxyURL, accessToken, accountID)
	if err != nil || !ok {
		return accounts.Usage{}, false, err
	}
	return u.Snapshot(), true, nil
}

// CurrentPlanUsage implements Workspace. Codex is the only provider that
// quotes rate limits on its responses, and the snapshot it publishes lives
// in the package that also carries its browser sign-in — which is exactly
// why the UI asks the workspace for it instead of reading it there.
func (w *AppWorkspace) CurrentPlanUsage(providerID string) (accounts.Usage, bool) {
	if providerID != codex.ProviderID {
		return accounts.Usage{}, false
	}
	u, ok := codex.LatestUsage()
	if !ok {
		return accounts.Usage{}, false
	}
	return u.Snapshot(), true
}

// CustomProviderTypes implements Workspace.
func (w *AppWorkspace) CustomProviderTypes() []string {
	return discover.RegisteredProviderTypes()
}

// KnownProviders implements Workspace.
func (w *AppWorkspace) KnownProviders() []catwalk.Provider {
	return w.store.KnownProviders()
}
