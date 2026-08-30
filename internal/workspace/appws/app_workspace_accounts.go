package appws

import (
	"context"
	"fmt"

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
// between calls. Every account-mutating method below shares this one
// helper instead of repeating the same accounts.NewFileStore call.
func (w *AppWorkspace) accountStore() *accounts.FileStore {
	return accounts.NewFileStore(config.GlobalAccountsFile())
}

// RecordAccount implements Workspace by delegating to config.RecordAccount.
// See accountStore's doc comment for why the store is opened fresh via that
// helper rather than cached on AppWorkspace.
func (w *AppWorkspace) RecordAccount(scope config.Scope, providerID string, cred accounts.LegacyCredential) (accounts.Account, error) {
	accStore := w.accountStore()
	a, err := config.RecordAccount(w.store, accStore, scope, providerID, cred)
	if err != nil {
		return accounts.Account{}, err
	}
	w.app.Credentials().SignalAuthComplete(providerID)
	return a, nil
}

// ListAccounts implements Workspace by delegating to the account store.
// See accountStore's comment for why the store is opened fresh here
// rather than cached on AppWorkspace. It first folds any pre-existing
// single credential into the store (config.EnsureAccountMigrated), so a
// user who authenticated before the multi-account feature existed sees
// their own account here instead of an empty list.
func (w *AppWorkspace) ListAccounts(providerID string) ([]accounts.Account, error) {
	accStore := w.accountStore()
	if err := config.EnsureAccountMigrated(w.store, accStore, providerID); err != nil {
		return nil, err
	}
	return accStore.List(providerID)
}

// ActivateAccount implements Workspace by looking up accountID and
// delegating the switch to config.ConfigStore.ActivateAccount.
func (w *AppWorkspace) ActivateAccount(scope config.Scope, providerID, accountID string) error {
	accStore := w.accountStore()
	a, ok, err := accStore.Get(providerID, accountID)
	if err != nil {
		return fmt.Errorf("looking up account %s for provider %s: %w", accountID, providerID, err)
	}
	if !ok {
		return fmt.Errorf("account %s not found for provider %s", accountID, providerID)
	}
	return w.store.ActivateAccount(scope, providerID, a)
}

// UpdateAccount implements Workspace by delegating to config.UpdateAccount.
// The account store is opened fresh here rather than cached on
// AppWorkspace — see accountStore's comment above for why. The rules
// themselves (upsert, then republish if active) live in config.UpdateAccount
// so this stays a thin wrapper: see that function's doc comment for why a
// second copy of the logic here would be the wrong shape.
func (w *AppWorkspace) UpdateAccount(providerID string, account accounts.Account) error {
	accStore := w.accountStore()
	return config.UpdateAccount(w.store, accStore, providerID, account)
}

// RemoveAccount implements Workspace by delegating to config.RemoveAccount,
// which owns the actual rules (refuse the last account, activate a
// replacement before deleting the active one) — see its doc comment for
// why those rules must not be duplicated here or in a test double.
func (w *AppWorkspace) RemoveAccount(scope config.Scope, providerID, accountID string) error {
	accStore := w.accountStore()
	return config.RemoveAccount(w.store, accStore, scope, providerID, accountID)
}

// SetProviderProxy implements Workspace by delegating to
// config.SetProviderProxy, exactly like UpdateAccount/RemoveAccount above.
func (w *AppWorkspace) SetProviderProxy(providerID, proxy string) error {
	accStore := w.accountStore()
	return config.SetProviderProxy(w.store, accStore, providerID, proxy)
}

// RefreshAccountLimits implements Workspace by delegating to
// config.RefreshAccountLimits, exactly like UpdateAccount/RemoveAccount
// above, and supplying the fetcher that knows how to ask a vendor.
//
// The wiring is here rather than in internal/config because reaching
// Codex's HTTP endpoint means importing the package that also carries its
// browser sign-in flow, and internal/config is imported by nearly
// everything. Codex is the only provider that reports usage today, and
// config's own guard (accounts.CapabilitiesOf) is what decides whether the
// fetcher is called at all.
func (w *AppWorkspace) RefreshAccountLimits(ctx context.Context, providerID string) ([]accounts.Account, error) {
	accStore := w.accountStore()
	return config.RefreshAccountLimits(ctx, w.store, accStore, providerID, fetchCodexUsage)
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
