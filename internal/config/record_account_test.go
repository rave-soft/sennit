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
)

func newRecordAccountStore(t *testing.T) *ConfigStore {
	t.Helper()
	return newActivateAccountTestStore(t, "")
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
		knownProviders: []catwalk.Provider{CodexProvider()},
		globalDataPath: filepath.Join(dir, "sennit.json"),
		resolver:       NewShellVariableResolver(env.New()),
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

	store := newRecordAccountStore(t)
	oldToken := fakeCodexJWT(t, "acct-old")
	provider, _ := store.Config().Providers.Get(codex.ProviderID)
	provider.APIKey = oldToken
	provider.OAuthToken = &oauth.Token{AccessToken: oldToken}
	store.Config().Providers.Set(codex.ProviderID, provider)

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

	activeProvider, ok := store.Config().Providers.Get(codex.ProviderID)
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

	activeProvider, ok := store.Config().Providers.Get(codex.ProviderID)
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
	require.Equal(t, token, provider.APIKey, "the new account should be active")

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
