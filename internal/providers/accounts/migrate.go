package accounts

import (
	"cmp"
	"fmt"

	"github.com/rave-soft/sennit/internal/oauth"
)

// LegacyCredential is the single-account credential shape the rest of
// Sennit used before this package existed — one API key or OAuth token
// per provider, no concept of multiple accounts.
//
// Migrate takes this rather than a config type (e.g.
// config.ProviderConfig) because accounts is a leaf package: internal/
// config is free to import accounts, but accounts must not import
// internal/config, on pain of an import cycle. LegacyCredential is the
// neutral shape that lets the dependency point one way only; the caller
// in internal/config (added in a later phase) is responsible for
// building one from its own provider config.
type LegacyCredential struct {
	// APIKey is the template as configured ("$VAR", "$(cmd)"), never a
	// resolved secret.
	APIKey string
	// Token is the OAuth token, for OAuth-authenticated providers.
	Token *oauth.Token
	// ProxyURL is carried over as-is, including the literal "none".
	ProxyURL string
	// AccountID is the provider's own account identifier, when the
	// caller can derive one (e.g. from a JWT claim).
	AccountID string
	// Email is for display only.
	Email string
	// Label is the caller-supplied display name. A default is used
	// when empty.
	Label string
	// ForceNewAccount tells config.RecordAccount that this credential is
	// a deliberate new sign-in (e.g. "Add account…" from the accounts
	// dialog), not a routine (re-)login. It only matters for a provider
	// with no AccountID of its own: such a provider has no identity to
	// recognize "the same sign-in" by, so it cannot otherwise distinguish
	// a forced re-authentication (which should update the existing
	// active account in place) from a deliberate second sign-in (which
	// should create a new one) — only the caller, which knows the user's
	// actual intent, can say which this is. Migrate ignores this field
	// entirely; it only affects RecordAccount.
	ForceNewAccount bool
}

// Migrate turns a provider's pre-existing single credential into the
// first account in store, if the provider has no accounts yet.
//
// It is idempotent: once a provider has any account, Migrate leaves the
// store untouched and reports false, so it is safe to call on every
// startup without re-checking first or risking clobbering an account the
// user has since edited.
//
// It reports (Account{}, false, nil), not an error, when there is
// nothing to migrate — cred has neither an API key nor a token, which is
// the ordinary case for a provider that's merely declared in config but
// never authenticated.
func Migrate(store Store, providerID string, cred LegacyCredential) (Account, bool, error) {
	existing, err := store.List(providerID)
	if err != nil {
		return Account{}, false, fmt.Errorf("list accounts for provider %q: %w", providerID, err)
	}
	if len(existing) > 0 {
		return Account{}, false, nil
	}

	hasAPIKey := cred.APIKey != ""
	hasToken := cred.Token != nil
	if !hasAPIKey && !hasToken {
		return Account{}, false, nil
	}

	label := cmp.Or(cred.Label, cred.Email, cred.AccountID, "Default")

	a := Account{
		ID:        NextID(nil, cred.AccountID),
		Label:     label,
		AccountID: cred.AccountID,
		Email:     cred.Email,
		ProxyURL:  cred.ProxyURL,
		Token:     cred.Token,
		APIKey:    cred.APIKey,
	}
	// Validate (via Upsert) is what catches the "both APIKey and Token
	// set" contradiction; no need to duplicate that check here.
	if err := store.Upsert(providerID, a); err != nil {
		return Account{}, false, fmt.Errorf("migrate legacy credential for provider %q: %w", providerID, err)
	}
	return a, true, nil
}
