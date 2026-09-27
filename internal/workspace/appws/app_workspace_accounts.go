package appws

import (
	"context"
	"fmt"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/discover"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/workspace"
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

// RecordAccount implements Workspace. workspace.AccountCredential carries no
// Token: an OAuth sign-in is recorded by completeCodexOAuth/
// completeCopilotOAuth below instead, straight through recordAccount, so a
// refresh token never has to cross this contract (CLIENT-SERVER.md PR 1.3).
// What is left for a frontend to call this with is an API-key credential
// (see internal/cmd/accounts.go's authAddAPIKey).
func (w *AppWorkspace) RecordAccount(scope config.Scope, providerID string, cred workspace.AccountCredential) (workspace.FrontendAccount, error) {
	return w.recordAccount(scope, providerID, accounts.LegacyCredential{
		APIKey:          cred.APIKey,
		ProxyURL:        cred.ProxyURL,
		AccountID:       cred.AccountID,
		Email:           cred.Email,
		Label:           cred.Label,
		ForceNewAccount: cred.ForceNewAccount,
	})
}

// recordAccount is the actual persistence RecordAccount and the OAuth
// completion helpers (app_workspace_oauth.go) share; unlike RecordAccount
// itself, it takes the full accounts.LegacyCredential, Token included, since
// it is never reached over the wire (see the doc comment above).
func (w *AppWorkspace) recordAccount(scope config.Scope, providerID string, cred accounts.LegacyCredential) (workspace.FrontendAccount, error) {
	a, err := w.accounts().Record(scope, providerID, cred)
	if err != nil {
		return workspace.FrontendAccount{}, err
	}
	w.app.Credentials().SignalAuthComplete(providerID)
	return workspace.NewFrontendAccount(a), nil
}

// ListAccounts implements Workspace.
func (w *AppWorkspace) ListAccounts(providerID string) ([]workspace.FrontendAccount, error) {
	accs, err := w.accounts().List(providerID)
	if err != nil {
		return nil, err
	}
	return frontendAccounts(accs), nil
}

// ActivateAccount implements Workspace.
func (w *AppWorkspace) ActivateAccount(scope config.Scope, providerID, accountID string) error {
	return w.accounts().Activate(scope, providerID, accountID)
}

// UpdateAccountFields implements Workspace. The stored account is loaded
// fresh and only the edited fields are applied - the credential (Token,
// APIKey) always comes from disk, never from the caller, so a frontend
// can never overwrite it by round-tripping a FrontendAccount it was
// handed (see CLIENT-SERVER.md PR 0.5c).
//
// edit.ProxyURL is never resolveSubmittedProxy'd (contrast StartOAuth/
// OAuthValidateProxy): every frontend caller of this method (account_form.go)
// only sets it when the typed value differs from the pre-filled, already-
// redacted FrontendAccount.ProxyURL, so a redacted value never reaches
// here in the first place - see account_form.go's submit(). The CLI's
// `sennit accounts proxy` reaches this with a value the user typed
// directly, never one read back off FrontendAccount/FrontendConfig.
func (w *AppWorkspace) UpdateAccountFields(providerID, accountID string, edit workspace.AccountEdit) error {
	store := w.accountStore()
	account, ok, err := store.Get(providerID, accountID)
	if err != nil {
		return fmt.Errorf("looking up account %s for provider %s: %w", accountID, providerID, err)
	}
	if !ok {
		return fmt.Errorf("account %s not found for provider %s", accountID, providerID)
	}
	if edit.Label != nil {
		account.Label = *edit.Label
	}
	if edit.ProxyURL != nil {
		account.ProxyURL = *edit.ProxyURL
	}
	if edit.Disabled != nil {
		account.Disabled = *edit.Disabled
	}
	return config.NewAccountsService(w.store, store, fetchCodexUsage).Update(providerID, account)
}

// frontendAccounts projects a slice of stored accounts down to what a
// frontend is allowed to see.
func frontendAccounts(accs []accounts.Account) []workspace.FrontendAccount {
	out := make([]workspace.FrontendAccount, len(accs))
	for i, a := range accs {
		out[i] = workspace.NewFrontendAccount(a)
	}
	return out
}

// RemoveAccount implements Workspace.
func (w *AppWorkspace) RemoveAccount(scope config.Scope, providerID, accountID string) error {
	return w.accounts().Remove(scope, providerID, accountID)
}

// PurgeAccounts implements Workspace.
func (w *AppWorkspace) PurgeAccounts(scope config.Scope, providerID string) error {
	return w.accounts().Purge(scope, providerID)
}

// SetProviderProxy implements Workspace. See UpdateAccountFields' doc
// comment: proxy is never resolveSubmittedProxy'd here either, for the same
// reason - provider_settings.go's submit() only sends a proxy edit when it
// differs from the pre-filled, already-redacted FrontendProvider.ProxyURL,
// and the CLI's `sennit accounts proxy` (with no account arg) reaches this
// with a value typed directly.
func (w *AppWorkspace) SetProviderProxy(providerID, proxy string) error {
	return w.accounts().SetProviderProxy(providerID, proxy)
}

// RefreshAccountLimits implements Workspace.
func (w *AppWorkspace) RefreshAccountLimits(ctx context.Context, providerID string) ([]workspace.FrontendAccount, error) {
	accs, err := w.accounts().RefreshLimits(ctx, providerID)
	if err != nil {
		return nil, err
	}
	return frontendAccounts(accs), nil
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

// AccountCapabilities converts the accounts package's capability record
// into the contract's own shape, so internal/ui can render the settings
// dialog without importing internal/providers/accounts for it.
func (w *AppWorkspace) AccountCapabilities(providerID string) workspace.AccountCapabilities {
	c := accounts.CapabilitiesOf(providerID)
	var rotateOn workspace.RotateOn
	switch {
	case c.RotateOn.RotatesOnThreshold() && c.RotateOn.RotatesOnRateLimit():
		rotateOn = workspace.RotateBoth
	case c.RotateOn.RotatesOnThreshold():
		rotateOn = workspace.RotateThreshold
	case c.RotateOn.RotatesOnRateLimit():
		rotateOn = workspace.RotateRateLimit
	}
	return workspace.AccountCapabilities{Usage: c.Usage, RotateOn: rotateOn, OAuth: c.AuthKind == accounts.AuthOAuth}
}
