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
//     Label, rather than growing a duplicate. This match wins regardless
//     of cred.ForceNewAccount — two logins to the literal same account
//     are the same account no matter which UI path the user took.
//  3. Otherwise, if cred.ForceNewAccount is set, the caller has already
//     decided this is a deliberate new sign-in (e.g. "Add account…" in
//     the accounts dialog), so step 4 below is skipped entirely and a new
//     account is always created.
//  4. Otherwise, if cred has no AccountID at all (the provider has no
//     identity of its own to key on) and the provider already has an
//     active account, that active account is updated in place instead of
//     creating a new one — see the comment on step 4 below for why.
//  5. Otherwise a new account is created with accounts.NextID.
//  6. The account is upserted into accStore, then made active via
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
	if !isUpdate && cred.AccountID == "" && !cred.ForceNewAccount {
		// A provider with no AccountID gives us nothing to recognize "the
		// same sign-in" by. Re-authentication is not rare for these
		// providers — Sennit itself triggers it whenever a refresh token
		// is rejected (see retryAfterUnauthorized in
		// internal/agent/runtime_builder.go), and the user just walks
		// back through this same login flow. Treating every such
		// re-login as a brand new account would silently grow the list
		// by one entry per forced re-auth, which is strictly worse than
		// the pre-accounts behavior (a re-login used to just overwrite
		// the one credential there was). So when there's an active
		// account already and no identity to tell logins apart, assume
		// this is that same account being refreshed and update it in
		// place. A deliberate second account for such a provider is
		// still possible — that's what "Add account…" is for, and it
		// sets cred.ForceNewAccount to skip this branch, since it can't
		// otherwise be told apart from a re-login.
		if id := activeAccountID(store, providerID); id != "" {
			a, isUpdate = findByID(existing, id)
		}
	}
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

// UpdateAccount saves account's user-editable fields (Label, ProxyURL,
// Disabled) to accStore. When account is providerID's currently active
// one, it additionally goes through ConfigStore.ActivateAccount: that is
// the one call that republishes an account into the live ProviderConfig
// (resolved credentials and effective proxy) that every outgoing request
// actually reads, so an edited proxy takes effect immediately instead of
// sitting unused until the next restart. A second, ad hoc publish path
// here would risk drifting from the one ActivateAccount already
// maintains — see its doc comment for why disk and memory have to be
// written in that order.
//
// This is the single implementation callers (AppWorkspace and any test
// double standing in for it) must go through — see the doc comment on
// RemoveAccount below for why a duplicated copy is the wrong shape here.
func UpdateAccount(store *ConfigStore, accStore accounts.Store, providerID string, account accounts.Account) error {
	if err := accStore.Upsert(providerID, account); err != nil {
		return fmt.Errorf("updating account %s for provider %s: %w", account.ID, providerID, err)
	}
	if pc, ok := store.Config().Providers.Get(providerID); ok && pc.Account == account.ID {
		if err := store.ActivateAccount(ScopeGlobal, providerID, account); err != nil {
			return fmt.Errorf("republishing updated active account %s for provider %s: %w", account.ID, providerID, err)
		}
	}
	return nil
}

// RemoveAccount deletes an account, subject to two rules: the last account
// for a provider can't be removed (that would leave credentials configured
// with nowhere to point — see the error below, which names `sennit
// logout` as the actual way to do that), and removing the active account
// activates a replacement first so the provider is never left pointing at
// a deleted one.
//
// This lives here — not duplicated in internal/workspace's AppWorkspace
// and again in a test double standing in for it — because a hand-copied
// second implementation of these rules is exactly the failure mode that
// bit this feature once already: an earlier revision of AppWorkspace's
// RemoveAccount and the workspace package's test-only ConfigAccessor each
// carried their own copy of "refuse the last account, activate a
// replacement before deleting," and the tests exercised only the copy in
// the test double. Breaking the real implementation left every test green.
// A single free function both call means there is only one place these
// rules can live, and a test against a real ConfigStore (see
// internal/config's own test for this) actually exercises what production
// runs.
func RemoveAccount(store *ConfigStore, accStore accounts.Store, scope Scope, providerID, accountID string) error {
	existing, err := accStore.List(providerID)
	if err != nil {
		return fmt.Errorf("listing accounts for provider %s: %w", providerID, err)
	}
	if len(existing) <= 1 {
		return fmt.Errorf("cannot remove the last account for provider %s: this would leave it signed in with no account to use — run `sennit logout` instead", providerID)
	}

	if pc, ok := store.Config().Providers.Get(providerID); ok && pc.Account == accountID {
		next, ok := nextAccountAfterRemoval(existing, accountID)
		if !ok {
			return fmt.Errorf("no replacement account found for provider %s", providerID)
		}
		if err := store.ActivateAccount(scope, providerID, next); err != nil {
			return fmt.Errorf("activating replacement account for provider %s: %w", providerID, err)
		}
	}

	if err := accStore.Remove(providerID, accountID); err != nil {
		return fmt.Errorf("removing account %s for provider %s: %w", accountID, providerID, err)
	}
	return nil
}

// nextAccountAfterRemoval picks the account RemoveAccount should activate
// before deleting excludeID: the first non-disabled account other than
// excludeID, or, if every other account is disabled, the first one
// regardless. Preferring an enabled account keeps the provider usable
// immediately; falling back to a disabled one still beats refusing the
// removal, since the user is free to re-enable it afterward.
func nextAccountAfterRemoval(existing []accounts.Account, excludeID string) (accounts.Account, bool) {
	var fallback accounts.Account
	haveFallback := false
	for _, a := range existing {
		if a.ID == excludeID {
			continue
		}
		if !a.Disabled {
			return a, true
		}
		if !haveFallback {
			fallback = a
			haveFallback = true
		}
	}
	return fallback, haveFallback
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

// findByID looks for an account with the given account store ID (not to be
// confused with the provider-side AccountID findByAccountID matches on).
func findByID(existing []accounts.Account, id string) (accounts.Account, bool) {
	for _, a := range existing {
		if a.ID == id {
			return a, true
		}
	}
	return accounts.Account{}, false
}

// activeAccountID returns providerID's currently active account ID, or ""
// if none is set. It reads the in-memory config: ActivateAccount publishes
// the active account ID to ProviderConfig.Account via UpdateProviderAccount
// (through AccountCredential.ActiveAccountID) in the same call that
// persists it to disk, so this is up to date immediately, with no reload
// required.
func activeAccountID(store *ConfigStore, providerID string) string {
	cfg := store.Config()
	if cfg == nil || cfg.Providers == nil {
		return ""
	}
	pc, ok := cfg.Providers.Get(providerID)
	if !ok {
		return ""
	}
	return pc.Account
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
