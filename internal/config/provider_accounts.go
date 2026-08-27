package config

// This file is the seam between the single-credential world ConfigStore
// grew up with and the multi-account one internal/providers/accounts adds
// on top: RecordAccount is what a login flow calls instead of writing
// straight into ProviderConfig, so a second sign-in adds an account rather
// than clobbering the first.

import (
	"cmp"
	"fmt"

	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/accounts"
)

// RecordAccount stores cred as an account for providerID and makes it the
// provider's active one, migrating a pre-existing single credential first
// so it is not lost.
//
// Order of operations matters here:
//  1. A provider that was already configured the old way (one api_key or
//     oauth token directly on ProviderConfig, no accounts yet) has that
//     credential migrated into accStore first. Without this, the first
//     call to RecordAccount for an already-authenticated provider would
//     silently drop whatever was configured before accounts.Store existed.
//     accounts.Migrate is idempotent, so this is safe to run on every call.
//  2. If an existing account already carries the same non-empty AccountID
//     as cred, this is a re-login to that account (e.g. a refreshed
//     token), not a new one: it is updated in place, keeping its ID and
//     Label, rather than growing a duplicate.
//  3. Otherwise a new account is created with accounts.NextID.
//  4. The account is upserted into accStore, then made active via
//     ActivateAccount.
func RecordAccount(store *ConfigStore, accStore accounts.Store, scope Scope, providerID string, cred accounts.LegacyCredential) (accounts.Account, error) {
	if _, _, err := accounts.Migrate(accStore, providerID, legacyCredentialFromProvider(store, providerID)); err != nil {
		return accounts.Account{}, fmt.Errorf("migrating existing credential for provider %s: %w", providerID, err)
	}

	existing, err := accStore.List(providerID)
	if err != nil {
		return accounts.Account{}, fmt.Errorf("listing accounts for provider %s: %w", providerID, err)
	}

	a, isUpdate := findByAccountID(existing, cred.AccountID)
	if isUpdate {
		// A re-login to an account we already know: keep its identity,
		// replace only what this sign-in actually refreshed. A login
		// flow that has no proxy opinion of its own (loginCodex passes
		// ProxyURL empty on purpose — see its comment) must not clear a
		// proxy the account already had: cmp.Or keeps a.ProxyURL when
		// cred.ProxyURL is empty, and only overwrites it when this
		// sign-in actually carried one. Deliberately removing an
		// account's proxy is account management (a direct accStore
		// operation, e.g. from the UI), not a side effect of signing in.
		a.Token = cred.Token
		a.APIKey = cred.APIKey
		a.ProxyURL = cmp.Or(cred.ProxyURL, a.ProxyURL)
	} else {
		a = accounts.Account{
			ID:        accounts.NextID(existing, cred.AccountID),
			Label:     cmp.Or(cred.Label, cred.Email, cred.AccountID, "Account"),
			AccountID: cred.AccountID,
			Email:     cred.Email,
			ProxyURL:  cred.ProxyURL,
			Token:     cred.Token,
			APIKey:    cred.APIKey,
		}
	}

	if err := accStore.Upsert(providerID, a); err != nil {
		return accounts.Account{}, fmt.Errorf("recording account for provider %s: %w", providerID, err)
	}

	// The account is already durably recorded at this point. If
	// activation fails below (e.g. an unresolvable api_key template),
	// deliberately do NOT roll the Upsert back: the account itself is
	// valid, and leaving it in place lets the user switch to it by hand
	// once the underlying problem (a missing env var, say) is fixed,
	// instead of losing the credential they just provided.
	if err := store.ActivateAccount(scope, providerID, a); err != nil {
		return a, fmt.Errorf("activating account for provider %s: %w", providerID, err)
	}
	return a, nil
}

// EnsureAccountMigrated makes sure providerID's pre-multi-account
// credential, if any, has been folded into accStore as an account — the
// read path's counterpart to RecordAccount's own migration step (see
// that function's doc comment, step 1). accounts.Migrate is a no-op once
// the provider already has at least one account on record (it checks
// store.List first), so calling this on every ListAccounts read is safe,
// not redundant work repeated every time.
//
// Unlike RecordAccount, a fresh migration here also has to mark the
// migrated account active: RecordAccount's caller always follows up with
// a real ActivateAccount call, but a bare read has no such step, and
// without this the provider's ProviderConfig.Account field would stay
// empty even though the provider's live APIKey/OAuthToken ARE that
// account's credentials — the UI would show a single account belonging
// to nobody, active marker on nothing.
func EnsureAccountMigrated(store *ConfigStore, accStore accounts.Store, providerID string) error {
	a, migrated, err := accounts.Migrate(accStore, providerID, legacyCredentialFromProvider(store, providerID))
	if err != nil {
		return fmt.Errorf("migrating existing credential for provider %s: %w", providerID, err)
	}
	if !migrated {
		return nil
	}
	if err := store.SetConfigField(ScopeGlobal, ProviderFieldKey(providerID, "account"), a.ID); err != nil {
		return fmt.Errorf("marking migrated account active for provider %s: %w", providerID, err)
	}
	return nil
}

// findByAccountID looks for an account already carrying the given
// provider-side AccountID. An empty accountID never matches — providers
// with nothing to key on (plain API keys) always get a new account rather
// than being merged into an unrelated one that also happens to lack an
// AccountID.
func findByAccountID(existing []accounts.Account, accountID string) (accounts.Account, bool) {
	if accountID == "" {
		return accounts.Account{}, false
	}
	for _, a := range existing {
		if a.AccountID == accountID {
			return a, true
		}
	}
	return accounts.Account{}, false
}

// legacyCredentialFromProvider builds the accounts.LegacyCredential that
// describes whatever single credential providerID currently has in
// ProviderConfig, for accounts.Migrate to fold into the account store.
// ProxyURL comes from ConfiguredProxyURL, not the (possibly
// account-overridden) effective ProxyURL, since a migrated account should
// carry the provider's own configured proxy, not whatever an in-memory
// switch last resolved it to.
func legacyCredentialFromProvider(store *ConfigStore, providerID string) accounts.LegacyCredential {
	cfg := store.Config()
	if cfg == nil || cfg.Providers == nil {
		return accounts.LegacyCredential{}
	}
	pc, ok := cfg.Providers.Get(providerID)
	if !ok {
		return accounts.LegacyCredential{}
	}

	cred := accounts.LegacyCredential{ProxyURL: pc.ConfiguredProxyURL}
	if pc.OAuthToken != nil {
		cred.Token = pc.OAuthToken
	} else {
		cred.APIKey = cmp.Or(pc.APIKeyTemplate, pc.APIKey)
	}

	// Only Codex can derive AccountID from what it already has on hand
	// (the chatgpt_account_id claim in its token); every other provider
	// leaves it empty rather than guessing.
	if providerID == codex.ProviderID {
		accountID := codex.AccountID(pc.APIKey)
		if accountID == "" && pc.OAuthToken != nil {
			accountID = codex.AccountID(pc.OAuthToken.AccessToken)
		}
		cred.AccountID = accountID
	}
	return cred
}
