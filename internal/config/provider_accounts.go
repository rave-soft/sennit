package config

// This file is the seam between the single-credential world ConfigStore
// grew up with and the multi-account one internal/providers/accounts adds
// on top: RecordAccount is what a login flow calls instead of writing
// straight into ProviderConfig, so a second sign-in adds an account rather
// than clobbering the first.

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"sync"

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
// Disabling the active account is the one edit that must NOT republish it:
// every other activation path (the accounts dialog, `sennit accounts use`)
// already refuses to activate a disabled account, and republishing it here
// would leave the provider pointing at a "Disabled" account that keeps
// serving live requests while Rotator.Pick, which only ever advances off
// the current account, never gets a reason to move away from it either. So
// a disable of the active account instead switches to
// nextAccountAfterRemoval's pick — the same "next usable account" logic
// RemoveAccount uses — falling back to leaving the provider with no active
// account at all when account was the only one on record.
//
// This is the single implementation callers (AppWorkspace and any test
// double standing in for it) must go through — see the doc comment on
// RemoveAccount below for why a duplicated copy is the wrong shape here.
func UpdateAccount(store *ConfigStore, accStore accounts.Store, providerID string, account accounts.Account) error {
	if err := accStore.Upsert(providerID, account); err != nil {
		return fmt.Errorf("updating account %s for provider %s: %w", account.ID, providerID, err)
	}
	pc, ok := store.Config().RuntimeProvider(providerID)
	if !ok || pc.Account != account.ID {
		return nil
	}

	if account.Disabled {
		existing, err := accStore.List(providerID)
		if err != nil {
			return fmt.Errorf("listing accounts for provider %s: %w", providerID, err)
		}
		next, hasNext := nextAccountAfterRemoval(existing, account.ID)
		if !hasNext {
			// No other account to fall back to: clear the active-account
			// pointer rather than leave it naming a disabled account.
			if err := store.RemoveConfigField(ScopeGlobal, ProviderFieldKey(providerID, "account")); err != nil {
				return fmt.Errorf("clearing active account pointer for provider %s: %w", providerID, err)
			}
			// RemoveConfigField's own reload is a best-effort autoReload
			// (skipped entirely when the store has no workingDir, e.g. a
			// bare ConfigStore built for tests) — mirror ActivateAccount's
			// explicit in-memory publish rather than depend on it, so the
			// live ProviderConfig.Account is cleared even when that reload
			// no-ops.
			store.mutateInMemory(func(c *Config) {
				if pc, ok := c.RuntimeProvider(providerID); ok {
					pc.Account = ""
					c.SetRuntimeProvider(providerID, pc)
				}
			})
			return nil
		}
		if err := store.ActivateAccount(ScopeGlobal, providerID, next); err != nil {
			return fmt.Errorf("activating replacement account for provider %s: %w", providerID, err)
		}
		return nil
	}

	if err := store.ActivateAccount(ScopeGlobal, providerID, account); err != nil {
		return fmt.Errorf("republishing updated active account %s for provider %s: %w", account.ID, providerID, err)
	}
	return nil
}

// SetProviderProxy sets providerID's provider-level proxy — the base
// UpdateAccount/ActivateAccount resolve an account's effective proxy
// against, exposed on disk as providers.<id>.proxy_url and in memory as
// the runtime provider's ConfiguredProxyURL — and republishes the active account's
// effective proxy so the change is live immediately.
//
// Without the republish step, a provider whose active account has no
// proxy of its own would keep sending requests through the OLD provider
// proxy (whatever ConfiguredProxyURL/ProxyURL happened to hold in memory)
// until the next full reload, even though the file on disk already has
// the new value — the same staleness UpdateProviderAccount's doc comment
// warns about for account switches. Passing an empty proxy removes the
// field entirely rather than writing an empty string, mirroring
// OAuthCodex.afterSave: an absent proxy_url and a present-but-empty one
// both mean "no provider proxy," but only the former is what a fresh
// config file would ever contain.
func SetProviderProxy(store *ConfigStore, accStore accounts.Store, providerID, proxy string) error {
	key := ProviderFieldKey(providerID, "proxy_url")
	var err error
	if proxy == "" {
		err = store.RemoveConfigField(ScopeGlobal, key)
	} else {
		err = store.SetConfigField(ScopeGlobal, key, proxy)
	}
	if err != nil {
		return fmt.Errorf("setting proxy for provider %s: %w", providerID, err)
	}

	pc, ok := store.Config().RuntimeProvider(providerID)
	if !ok || pc.Account == "" {
		// No active account to republish for yet (e.g. the provider
		// hasn't been signed into) — the reload SetConfigField already
		// triggered picked up the new ConfiguredProxyURL, and that's all
		// there is to do.
		return nil
	}
	account, ok, err := accStore.Get(providerID, pc.Account)
	if err != nil {
		return fmt.Errorf("looking up active account for provider %s: %w", providerID, err)
	}
	if !ok {
		return nil
	}
	// Re-activating the already-active account is the existing, correct
	// way to recompute its effective proxy against the just-updated
	// ConfiguredProxyURL: ActivateAccount republishes credentials AND
	// proxy together via UpdateProviderAccount, which is exactly what's
	// needed here too — a call carrying only a proxy override would have
	// to reconstruct the resolved API key/token itself (see
	// UpdateProviderAccount's unconditional
	// provider.APIKey = cred.APIKey), duplicating logic ActivateAccount
	// already owns.
	if err := store.ActivateAccount(ScopeGlobal, providerID, account); err != nil {
		return fmt.Errorf("republishing effective proxy for provider %s: %w", providerID, err)
	}
	return nil
}

// refreshAccountLimitsConcurrency bounds how many accounts' usage is
// fetched at once. A provider rarely has more than a handful of accounts,
// but nothing stops a "refresh limits" press from firing off dozens of
// concurrent requests without a cap.
const refreshAccountLimitsConcurrency = 4

// AccountUsageFetcher fetches one account's current rate-limit snapshot.
// It reports (Usage{}, false, nil) when the provider answered without usage
// headers, which is a normal outcome and not an error.
//
// The fetcher is a parameter rather than a package-level call so this file
// does not have to know which provider reports usage or how. Today that is
// Codex, and the wiring lives in internal/workspace/appws where the sign-in
// packages already are: config describes accounts, and reaching a vendor's
// HTTP endpoint from here would put a browser OAuth flow in the dependency
// cone of a package that nearly everything imports.
type AccountUsageFetcher func(ctx context.Context, proxyURL, accessToken, accountID string) (accounts.Usage, bool, error)

// RefreshAccountLimits fetches a fresh rate-limit snapshot for every OAuth
// account of providerID and persists it into accStore, then returns the
// provider's accounts reflecting whatever was learned. Providers that
// don't report usage (accounts.CapabilitiesOf(providerID).Usage false)
// are a no-op: their accounts are returned unchanged.
//
// Accounts are refreshed concurrently, bounded by
// refreshAccountLimitsConcurrency, each against its own effective proxy
// (accounts.ResolveProxy) and its own access token. A fetch that errors or
// comes back with no usage headers (fetch's own (Usage{}, false, nil)
// case — including a 401 from a token that needs refreshing, which is out
// of scope here) leaves that account's stored snapshot untouched instead
// of failing the whole refresh: the point of this call is comparing
// accounts, and one bad account must not hide the others' numbers. Only a
// failure to even list the provider's accounts is reported as an error.
//
// Only OAuth accounts (accounts.AuthOAuth) have a bearer token to send;
// an API-key account is skipped. Today the only Usage provider (Codex) is
// also the only OAuth one, so this is not specially guarded — a provider
// combining Usage with a non-OAuth AuthKind would simply have nothing to
// refresh.
func RefreshAccountLimits(ctx context.Context, store *ConfigStore, accStore accounts.Store, providerID string, fetch AccountUsageFetcher) ([]accounts.Account, error) {
	if !accounts.CapabilitiesOf(providerID).Usage {
		return accStore.List(providerID)
	}

	existing, err := accStore.List(providerID)
	if err != nil {
		return nil, fmt.Errorf("listing accounts for provider %s: %w", providerID, err)
	}

	var providerProxy string
	if pc, ok := store.Config().RuntimeProvider(providerID); ok {
		providerProxy = pc.ConfiguredProxyURL
	}

	sem := make(chan struct{}, refreshAccountLimitsConcurrency)
	var wg sync.WaitGroup
	for _, a := range existing {
		if a.Token == nil {
			continue // api-key account: no bearer token to fetch usage with
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(a accounts.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			proxy := accounts.ResolveProxy(a.ProxyURL, providerProxy)
			u, ok, err := fetch(ctx, proxy, a.Token.AccessToken, a.AccountID)
			if err != nil || !ok {
				return // leave the stored snapshot untouched
			}
			if err := accStore.RecordUsage(providerID, a.ID, u); err != nil {
				// The fetch worked and the numbers are simply lost:
				// worth a line, but not worth failing a refresh whose
				// other accounts may well have been written.
				slog.Warn("Failed to store refreshed account limits",
					"provider", providerID, "account", a.ID, "error", err)
			}
		}(a)
	}
	wg.Wait()

	return accStore.List(providerID)
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
// RemoveAccount and the workspace package's test-only adapter each carried
// their own copy of "refuse the last account, activate a
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

	if pc, ok := store.Config().RuntimeProvider(providerID); ok && pc.Account == accountID {
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

// PurgeAccounts deletes every account on record for providerID, bypassing
// RemoveAccount's last-account guard. RemoveAccount refuses to go below one
// account so a provider is never left "configured" with nowhere to point;
// that is exactly wrong for a full sign-out — `sennit logout` means to
// leave the provider with zero accounts, not one. Without this, the last
// account's OAuth token sat untouched in accStore forever (RemoveAccount
// pointed the user at `sennit logout`, which never actually called it),
// and `sennit accounts use` could silently resurrect the "logged out"
// session by reactivating it.
//
// It also clears providers.<id>.account, the pointer at the active
// account's ID: left behind, it would name an account that no longer
// exists.
func PurgeAccounts(store *ConfigStore, accStore accounts.Store, scope Scope, providerID string) error {
	existing, err := accStore.List(providerID)
	if err != nil {
		return fmt.Errorf("listing accounts for provider %s: %w", providerID, err)
	}
	for _, a := range existing {
		if err := accStore.Remove(providerID, a.ID); err != nil {
			return fmt.Errorf("removing account %s for provider %s: %w", a.ID, providerID, err)
		}
	}
	if err := store.RemoveConfigField(scope, ProviderFieldKey(providerID, "account")); err != nil {
		return fmt.Errorf("clearing active account pointer for provider %s: %w", providerID, err)
	}
	return nil
}

// nextAccountAfterRemoval picks the first enabled account other than
// excludeID. A disabled account must never become active: activation would
// silently bypass the disabled state and keep serving requests.
func nextAccountAfterRemoval(existing []accounts.Account, excludeID string) (accounts.Account, bool) {
	for _, a := range existing {
		if a.ID != excludeID && !a.Disabled {
			return a, true
		}
	}
	return accounts.Account{}, false
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
	// Runs either way: an account just folded in from a pre-accounts
	// credential has no email for the same reason an older recorded one
	// does not — nobody read it out of the token.
	if err := backfillCodexIdentity(accStore, providerID); err != nil {
		return err
	}
	if !migrated {
		return nil
	}
	if err := store.SetConfigField(ScopeGlobal, ProviderFieldKey(providerID, "account"), a.ID); err != nil {
		return fmt.Errorf("marking migrated account active for provider %s: %w", providerID, err)
	}
	return nil
}

// backfillCodexIdentity fills in the email of Codex accounts recorded
// before it was read from the token, and replaces a label that is nothing
// but the account's UUID.
//
// Both are display-only, and both have been in the token on disk the whole
// time: the sign-in that stored it simply did not look. Without this an
// account recorded earlier keeps showing its UUID until the next full
// re-login, which for Codex happens only when a refresh token is finally
// rejected — so, for most people, not for a long while.
//
// A label is replaced only when it is exactly the AccountID. That is the
// automatic fallback RecordAccount uses when it has nothing better; any
// other label was either derived from something the token said or typed by
// the person, and neither is ours to overwrite.
func backfillCodexIdentity(accStore accounts.Store, providerID string) error {
	if providerID != codex.ProviderID {
		return nil
	}
	list, err := accStore.List(providerID)
	if err != nil {
		return fmt.Errorf("listing accounts for provider %s: %w", providerID, err)
	}
	for _, a := range list {
		if a.Token == nil || a.Email != "" {
			continue
		}
		email := codex.Email(a.Token.AccessToken)
		if email == "" {
			continue
		}
		a.Email = email
		if a.Label == a.AccountID {
			a.Label = email
		}
		if err := accStore.Upsert(providerID, a); err != nil {
			return fmt.Errorf("backfilling account %s for provider %s: %w", a.ID, providerID, err)
		}
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
	pc, ok := cfg.RuntimeProvider(providerID)
	if !ok {
		return ""
	}
	return pc.Account
}

// legacyCredentialFromProvider builds the accounts.LegacyCredential that
// describes whatever single credential providerID currently has, for
// accounts.Migrate to fold into the account store. Every credential field
// comes off RuntimeProvider, the only view that carries one: Providers is
// consulted only to confirm the provider has a config entry at all.
// ProxyURL comes from ConfiguredProxyURL, not the (possibly
// account-overridden) effective ProxyURL, since a migrated account should
// carry the provider's own configured proxy, not whatever an in-memory
// switch last resolved it to.
func legacyCredentialFromProvider(store *ConfigStore, providerID string) accounts.LegacyCredential {
	cfg := store.Config()
	if cfg == nil || cfg.Providers == nil {
		return accounts.LegacyCredential{}
	}
	if _, ok := cfg.Providers.Get(providerID); !ok {
		return accounts.LegacyCredential{}
	}
	runtimeProvider, ok := cfg.RuntimeProvider(providerID)
	if !ok {
		return accounts.LegacyCredential{}
	}

	cred := accounts.LegacyCredential{ProxyURL: runtimeProvider.ConfiguredProxyURL}
	if runtimeProvider.OAuthToken != nil {
		cred.Token = runtimeProvider.OAuthToken
	} else {
		cred.APIKey = cmp.Or(runtimeProvider.APIKeyTemplate, runtimeProvider.APIKey)
	}

	// Only Codex can derive AccountID from what it already has on hand
	// (the chatgpt_account_id claim in its token); every other provider
	// leaves it empty rather than guessing.
	if providerID == codex.ProviderID {
		accountID := codex.AccountID(runtimeProvider.APIKey)
		if accountID == "" && runtimeProvider.OAuthToken != nil {
			accountID = codex.AccountID(runtimeProvider.OAuthToken.AccessToken)
		}
		cred.AccountID = accountID
	}
	return cred
}
