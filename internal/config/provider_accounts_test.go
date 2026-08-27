package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/rave-soft/sennit/internal/oauth/codex"
)

// TestEnsureAccountMigrated_MigratesLegacyCredentialAndActivatesIt is the
// regression test for the upgrade path: a user who signed in before the
// multi-account feature existed has a credential sitting directly on
// ProviderConfig, and nothing in accounts.json. ListAccounts (via this
// function) must surface that credential as an account, and mark it
// active, rather than showing an empty list.
func TestEnsureAccountMigrated_MigratesLegacyCredentialAndActivatesIt(t *testing.T) {
	t.Parallel()

	store := newRecordAccountStore(t)
	token := fakeCodexJWT(t, "acct-legacy")
	provider, _ := store.Config().Providers.Get(codex.ProviderID)
	provider.APIKey = token
	store.Config().Providers.Set(codex.ProviderID, provider)

	accStore := newTestAccountsStore(t)

	require.NoError(t, EnsureAccountMigrated(store, accStore, codex.ProviderID))

	all, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, all, 1, "the legacy credential should have been migrated into a single account")
	require.Equal(t, token, all[0].APIKey)

	data, err := os.ReadFile(store.globalDataPath)
	require.NoError(t, err)
	require.Equal(t, all[0].ID, gjson.GetBytes(data, "providers.codex.account").String(), "the migrated account must be marked active")
}

// TestEnsureAccountMigrated_NoOpOnceAccountsExist covers both the case of
// an already-migrated provider and calling this repeatedly: neither
// should create a duplicate account or otherwise change anything.
func TestEnsureAccountMigrated_NoOpOnceAccountsExist(t *testing.T) {
	t.Parallel()

	store := newRecordAccountStore(t)
	token := fakeCodexJWT(t, "acct-legacy")
	provider, _ := store.Config().Providers.Get(codex.ProviderID)
	provider.APIKey = token
	store.Config().Providers.Set(codex.ProviderID, provider)

	accStore := newTestAccountsStore(t)

	require.NoError(t, EnsureAccountMigrated(store, accStore, codex.ProviderID))
	first, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, first, 1)

	// A second call must be a no-op: no duplicate account, same active
	// marker.
	require.NoError(t, EnsureAccountMigrated(store, accStore, codex.ProviderID))
	second, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Equal(t, first, second, "a repeat call must not change the account store")
}

// TestEnsureAccountMigrated_NoCredentialNoOp covers a provider that was
// never authenticated at all: there is nothing to migrate, and no
// account should be manufactured out of an empty ProviderConfig.
func TestEnsureAccountMigrated_NoCredentialNoOp(t *testing.T) {
	t.Parallel()

	store := newRecordAccountStore(t)
	accStore := newTestAccountsStore(t)

	require.NoError(t, EnsureAccountMigrated(store, accStore, codex.ProviderID))

	all, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Empty(t, all)
}
