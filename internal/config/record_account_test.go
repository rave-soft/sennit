package config

import (
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/env"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/providers/state"
)

func newRecordAccountStore(t *testing.T) *ConfigStore {
	t.Helper()
	return newActivateAccountTestStore(t, "")
}

// newRecordAccountStoreWithLegacyCredential mirrors newActivateAccountTestStore,
// but pre-populates the codex provider's legacy, pre-account credential
// (APIKey/OAuthToken) on both Providers and RuntimeProviders before the
// store is ever constructed. A real load would compile the identical value
// into RuntimeProviders too (see providerload/loader.go), so both are set
// here to match. Setting them up front — rather than fetching
// store.Config() and mutating it — matters because Providers is frozen the
// moment a Config is published, and this literal *ConfigStore construction
// is standing in for that publication.
func newRecordAccountStoreWithLegacyCredential(t *testing.T, token string) *ConfigStore {
	t.Helper()
	dir := t.TempDir()
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set(codex.ProviderID, ProviderConfig{
		ID:         codex.ProviderID,
		APIKey:     token,
		OAuthToken: &oauth.Token{AccessToken: token},
	})
	runtimeProviders := csync.NewMap[string, state.Provider]()
	runtimeProviders.Set(codex.ProviderID, state.Provider{
		ID:         codex.ProviderID,
		APIKey:     token,
		OAuthToken: &oauth.Token{AccessToken: token},
	})
	return &ConfigStore{
		config:         &Config{Providers: providers, RuntimeProviders: runtimeProviders},
		globalDataPath: filepath.Join(dir, "sennit.json"),
		resolver:       NewShellVariableResolver(env.New()),
		processor:      testRuntimeProcessor{},
	}
}

// newCleanMachineStore mimics a fresh install: no providers.codex entry in
// the config at all (that entry is only ever created by a sign-in), but
// the embedded catalog still knows Codex exists — matching
// ConfigStore.KnownProviders() on any real install.
func newCleanMachineStore(t *testing.T) *ConfigStore {
	t.Helper()
	dir := t.TempDir()
	return &ConfigStore{
		config:         &Config{Providers: csync.NewMap[string, ProviderConfig]()},
		knownProviders: []catwalk.Provider{{ID: catwalk.InferenceProvider(codex.ProviderID), Name: codex.ProviderName, APIEndpoint: codex.APIBaseURL, Type: catwalk.TypeOpenAI}},
		globalDataPath: filepath.Join(dir, "sennit.json"),
		resolver:       NewShellVariableResolver(env.New()),
		processor:      testRuntimeProcessor{},
	}
}

func newTestAccountsStore(t *testing.T) accounts.Store {
	t.Helper()
	return accounts.NewFileStore(t.TempDir() + "/accounts.json")
}

// TestRecordAccount_MigratesExistingAndAddsNew is the scenario the whole
// step exists for: a provider already signed in the old, single-credential
// way must not lose that credential when a second account is recorded —
// it becomes the first account, and the new one becomes active alongside
// it.
func TestRecordAccount_MigratesExistingAndAddsNew(t *testing.T) {
	t.Parallel()

	oldToken := fakeCodexJWT(t, "acct-old")
	store := newRecordAccountStoreWithLegacyCredential(t, oldToken)

	accStore := newTestAccountsStore(t)
	newToken := fakeCodexJWT(t, "acct-new")
	newAccount, err := RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:     &oauth.Token{AccessToken: newToken},
		AccountID: "acct-new",
	})
	require.NoError(t, err)

	all, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, all, 2, "the pre-existing credential should have been migrated alongside the new account")

	var sawOld, sawNew bool
	for _, a := range all {
		switch a.AccountID {
		case "acct-old":
			sawOld = true
		case "acct-new":
			sawNew = true
			require.Equal(t, newAccount.ID, a.ID)
		}
	}
	require.True(t, sawOld, "the migrated account should carry the old token's account ID")
	require.True(t, sawNew)

	activeProvider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, newToken, activeProvider.APIKey, "the new account should be the one published to the runtime")

	data, err := os.ReadFile(store.globalDataPath)
	require.NoError(t, err)
	require.Equal(t, newAccount.ID, gjson.GetBytes(data, "providers.codex.account").String())
}

// TestRecordAccount_ReloginSameAccountUpdatesInPlace covers the ordinary
// case of running `sennit login codex` again for an account already known:
// a token refresh, not a second account.
func TestRecordAccount_ReloginSameAccountUpdatesInPlace(t *testing.T) {
	t.Parallel()

	store := newRecordAccountStore(t)
	accStore := newTestAccountsStore(t)

	firstToken := fakeCodexJWT(t, "acct-same")
	first, err := RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:     &oauth.Token{AccessToken: firstToken},
		AccountID: "acct-same",
		Label:     "My Account",
	})
	require.NoError(t, err)

	all, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, all, 1)

	secondToken := fakeCodexJWT(t, "acct-same")
	second, err := RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:     &oauth.Token{AccessToken: secondToken},
		AccountID: "acct-same",
		Label:     "Ignored on relogin",
	})
	require.NoError(t, err)

	all, err = accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, all, 1, "a relogin to the same account must not create a duplicate")
	require.Equal(t, first.ID, second.ID, "the account's ID must survive a relogin")
	require.Equal(t, "My Account", all[0].Label, "the account's label must survive a relogin")
	require.Equal(t, secondToken, all[0].Token.AccessToken, "the credential itself must be refreshed")

	activeProvider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, secondToken, activeProvider.APIKey)
}

// TestRecordAccount_NoExistingCredentialSkipsMigration covers a provider
// with nothing configured yet: RecordAccount must not manufacture a
// migrated placeholder account out of an empty ProviderConfig.
func TestRecordAccount_NoExistingCredentialSkipsMigration(t *testing.T) {
	t.Parallel()

	store := newRecordAccountStore(t)
	accStore := newTestAccountsStore(t)

	token := fakeCodexJWT(t, "acct-fresh")
	_, err := RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:     &oauth.Token{AccessToken: token},
		AccountID: "acct-fresh",
	})
	require.NoError(t, err)

	all, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, all, 1, "an unconfigured provider has nothing to migrate")
}

// TestRecordAccount_CreatesProviderEntryOnCleanMachine is the regression
// test for the first-ever `sennit login codex` on a fresh install: the
// config has no providers.codex entry at all, since nothing has signed in
// yet, and RecordAccount (via ActivateAccount -> UpdateProviderAccount)
// must fabricate one from the catalog exactly as SetProviderAPIKey always
// has, rather than failing with "provider not found".
func TestRecordAccount_CreatesProviderEntryOnCleanMachine(t *testing.T) {
	t.Parallel()

	store := newCleanMachineStore(t)
	accStore := newTestAccountsStore(t)

	_, ok := store.Config().Providers.Get(codex.ProviderID)
	require.False(t, ok, "test setup should start with no providers.codex entry")

	token := fakeCodexJWT(t, "acct-clean")
	account, err := RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:     &oauth.Token{AccessToken: token},
		AccountID: "acct-clean",
	})
	require.NoError(t, err)

	provider, ok := store.Config().Providers.Get(codex.ProviderID)
	require.True(t, ok, "the provider entry should have been created from the catalog")
	require.Equal(t, codex.ProviderName, provider.Name)
	require.Equal(t, codex.APIBaseURL, provider.BaseURL)
	require.Equal(t, catwalk.TypeOpenAI, provider.Type)
	runtimeProvider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, token, runtimeProvider.APIKey, "the new account should be active")

	all, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, account.ID, all[0].ID)
}

// TestRecordAccount_ReloginPreservesAccountProxy is the regression test
// for a re-login clearing a proxy the account already had: loginCodex
// deliberately passes an empty ProxyURL on refresh (see its comment on
// why), and that must mean "unchanged", not "cleared".
func TestRecordAccount_ReloginPreservesAccountProxy(t *testing.T) {
	t.Parallel()

	store := newRecordAccountStore(t)
	accStore := newTestAccountsStore(t)

	firstToken := fakeCodexJWT(t, "acct-proxy")
	_, err := RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:     &oauth.Token{AccessToken: firstToken},
		AccountID: "acct-proxy",
		ProxyURL:  "http://account-proxy:8080",
	})
	require.NoError(t, err)

	secondToken := fakeCodexJWT(t, "acct-proxy")
	_, err = RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:     &oauth.Token{AccessToken: secondToken},
		AccountID: "acct-proxy",
		ProxyURL:  "",
	})
	require.NoError(t, err)

	all, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, "http://account-proxy:8080", all[0].ProxyURL, "a relogin with no proxy opinion must not clear the account's proxy")
}

// TestRecordAccount_NoAccountIDReloginUpdatesActiveInPlace covers a
// provider with no identity of its own to key logins on (e.g. Copilot).
// Sennit forces re-authentication for these providers whenever a refresh
// token is rejected, so a re-login through this path is routine, not a
// deliberate "add a second account" — it must refresh the already-active
// account in place rather than growing the list by one every time.
func TestRecordAccount_NoAccountIDReloginUpdatesActiveInPlace(t *testing.T) {
	t.Parallel()

	store := newRecordAccountStore(t)
	accStore := newTestAccountsStore(t)

	firstToken := fakeCodexJWT(t, "")
	first, err := RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token: &oauth.Token{AccessToken: firstToken},
		Label: "My Account",
	})
	require.NoError(t, err)

	all, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, all, 1)

	secondToken := fakeCodexJWT(t, "")
	second, err := RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:    &oauth.Token{AccessToken: secondToken},
		Label:    "Ignored on relogin",
		ProxyURL: "",
	})
	require.NoError(t, err)

	all, err = accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, all, 1, "a forced re-login with no account identity must not create a duplicate")
	require.Equal(t, first.ID, second.ID, "the account's ID must survive a relogin")
	require.Equal(t, "My Account", all[0].Label, "the account's label must survive a relogin")
	require.Equal(t, secondToken, all[0].Token.AccessToken, "the credential itself must be refreshed")

	activeProvider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, secondToken, activeProvider.APIKey, "the account remains active")
}

// TestRecordAccount_ForceNewAccountCreatesSecondAccount covers "Add
// account…" for a provider with no AccountID of its own: even though an
// account is already active, ForceNewAccount tells RecordAccount this is
// a deliberate new sign-in, so it must create a genuinely new account
// rather than refreshing the existing one in place.
func TestRecordAccount_ForceNewAccountCreatesSecondAccount(t *testing.T) {
	t.Parallel()

	store := newRecordAccountStore(t)
	accStore := newTestAccountsStore(t)

	firstToken := fakeCodexJWT(t, "")
	first, err := RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token: &oauth.Token{AccessToken: firstToken},
		Label: "First Account",
	})
	require.NoError(t, err)

	all, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, all, 1)

	secondToken := fakeCodexJWT(t, "")
	second, err := RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:           &oauth.Token{AccessToken: secondToken},
		Label:           "Second Account",
		ForceNewAccount: true,
	})
	require.NoError(t, err)

	all, err = accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, all, 2, "a deliberate add must create a new account, not update the existing one")
	require.NotEqual(t, first.ID, second.ID)

	activeProvider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, secondToken, activeProvider.APIKey, "the newly added account becomes active")
}

// TestRecordAccount_NoAccountIDNoActiveCreatesNew covers the first-ever
// sign-in for a provider with no account identity: there is nothing active
// yet to update in place, so a new account is created as usual.
func TestRecordAccount_NoAccountIDNoActiveCreatesNew(t *testing.T) {
	t.Parallel()

	store := newRecordAccountStore(t)
	accStore := newTestAccountsStore(t)

	token := fakeCodexJWT(t, "")
	account, err := RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token: &oauth.Token{AccessToken: token},
	})
	require.NoError(t, err)

	all, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, account.ID, all[0].ID)
}
