package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/accounts"
)

// TestPurgeAccounts_RemovesEvenTheLastAccount guards `sennit logout`'s
// contract: unlike RemoveAccount, which refuses to go below one account so
// a provider is never left "configured" with nowhere to point, a full
// sign-out means leaving zero accounts. Before this existed, the last
// account's OAuth token had no way to be dropped — RemoveAccount pointed
// callers at `sennit logout`, which never actually removed it — so
// `sennit accounts use` could resurrect a "logged out" session.
func TestPurgeAccounts_RemovesEvenTheLastAccount(t *testing.T) {
	t.Parallel()

	store := newRecordAccountStore(t)
	accStore := newTestAccountsStore(t)

	token := fakeCodexJWT(t, "acct-only")
	_, err := RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:     &oauth.Token{AccessToken: token},
		AccountID: "acct-only",
	})
	require.NoError(t, err)

	all, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, all, 1)

	// RemoveAccount refuses this (that's the guard PurgeAccounts must
	// bypass).
	err = RemoveAccount(store, accStore, ScopeGlobal, codex.ProviderID, all[0].ID)
	require.Error(t, err)

	require.NoError(t, PurgeAccounts(store, accStore, ScopeGlobal, codex.ProviderID))

	all, err = accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Empty(t, all, "PurgeAccounts must remove the last account, unlike RemoveAccount")

	data, err := os.ReadFile(store.globalDataPath)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(data, "providers.codex.account").Exists(),
		"the active-account pointer must be cleared so it doesn't name a deleted account")
}

// TestPurgeAccounts_MultipleAccountsAllRemoved covers the ordinary
// multi-account case: every account goes, not just the active one.
func TestPurgeAccounts_MultipleAccountsAllRemoved(t *testing.T) {
	t.Parallel()

	store := newRecordAccountStore(t)
	accStore := newTestAccountsStore(t)

	firstToken := fakeCodexJWT(t, "acct-first")
	_, err := RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:     &oauth.Token{AccessToken: firstToken},
		AccountID: "acct-first",
	})
	require.NoError(t, err)

	secondToken := fakeCodexJWT(t, "acct-second")
	_, err = RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:           &oauth.Token{AccessToken: secondToken},
		AccountID:       "acct-second",
		ForceNewAccount: true,
	})
	require.NoError(t, err)

	all, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, all, 2)

	require.NoError(t, PurgeAccounts(store, accStore, ScopeGlobal, codex.ProviderID))

	all, err = accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Empty(t, all)
}
