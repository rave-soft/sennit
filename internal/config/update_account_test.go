package config

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/accounts"
)

// TestUpdateAccount_DisablingActiveAccountSwitchesToReplacement is the
// regression test for UpdateAccount republishing an account the user just
// disabled: disabling the active account must hand activation to another,
// non-disabled account instead, exactly like RemoveAccount already does
// when the account being removed is the active one.
func TestUpdateAccount_DisablingActiveAccountSwitchesToReplacement(t *testing.T) {
	t.Parallel()

	store := newRecordAccountStore(t)
	accStore := newTestAccountsStore(t)

	first, err := RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:     &oauth.Token{AccessToken: fakeCodexJWT(t, "acct-first")},
		AccountID: "acct-first",
	})
	require.NoError(t, err)

	second, err := RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:           &oauth.Token{AccessToken: fakeCodexJWT(t, "acct-second")},
		AccountID:       "acct-second",
		ForceNewAccount: true,
	})
	require.NoError(t, err)

	pc, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, second.ID, pc.Account, "sanity: the second sign-in is the active one")

	second.Disabled = true
	require.NoError(t, UpdateAccount(store, accStore, codex.ProviderID, second))

	pc, ok = store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, first.ID, pc.Account,
		"disabling the active account must switch the provider to another, non-disabled account")

	all, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)
	for _, a := range all {
		if a.ID == second.ID {
			require.True(t, a.Disabled, "the edit itself must still be saved")
		}
	}
}

// TestUpdateAccount_DisablingTheOnlyAccountClearsActivePointer covers the
// case RemoveAccount refuses to handle at all (it won't remove the last
// account): disabling the only account leaves nothing to switch to, so the
// provider must end up with no active account rather than a republished,
// disabled one.
func TestUpdateAccount_DisablingTheOnlyAccountClearsActivePointer(t *testing.T) {
	t.Parallel()

	store := newRecordAccountStore(t)
	accStore := newTestAccountsStore(t)

	only, err := RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:     &oauth.Token{AccessToken: fakeCodexJWT(t, "acct-only")},
		AccountID: "acct-only",
	})
	require.NoError(t, err)

	only.Disabled = true
	require.NoError(t, UpdateAccount(store, accStore, codex.ProviderID, only))

	pc, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Empty(t, pc.Account,
		"disabling the only account must leave the provider with no active account, not a republished disabled one")
}

// TestUpdateAccount_NonDisabledEditToActiveAccountStillRepublishes guards
// the existing behavior UpdateAccount must keep for every edit that does
// not disable the active account: it still goes through ActivateAccount so
// the change (here, a new proxy) takes effect immediately.
func TestUpdateAccount_NonDisabledEditToActiveAccountStillRepublishes(t *testing.T) {
	t.Parallel()

	store := newRecordAccountStore(t)
	accStore := newTestAccountsStore(t)

	active, err := RecordAccount(store, accStore, ScopeGlobal, codex.ProviderID, accounts.LegacyCredential{
		Token:     &oauth.Token{AccessToken: fakeCodexJWT(t, "acct-active")},
		AccountID: "acct-active",
	})
	require.NoError(t, err)

	active.ProxyURL = "socks5://edited-proxy:1080"
	require.NoError(t, UpdateAccount(store, accStore, codex.ProviderID, active))

	pc, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, active.ID, pc.Account, "the account must remain active")
	require.Equal(t, "socks5://edited-proxy:1080", pc.ProxyURL,
		"a non-disabling edit to the active account must still republish through ActivateAccount")
}
