package config

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/accounts"
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

// TestRefreshAccountLimits_PersistsSuccessAndSkipsFailure is the core
// contract of the "refresh limits" action: every OAuth account is asked,
// a successful fetch is persisted, and a failing one leaves the account's
// existing snapshot alone instead of failing the whole call or clobbering
// it with a blank one.
func TestRefreshAccountLimits_PersistsSuccessAndSkipsFailure(t *testing.T) {
	t.Parallel()

	store := newRecordAccountStore(t)
	accStore := newTestAccountsStore(t)

	staleUsage := accounts.Usage{Plan: "stale", Primary: accounts.UsageWindow{UsedPercent: 50, WindowMinutes: 60 * 24 * 7}}
	require.NoError(t, accStore.Upsert(codex.ProviderID, accounts.Account{
		ID: "good", AccountID: "acct-good", Token: &oauth.Token{AccessToken: "tok-good"},
	}))
	require.NoError(t, accStore.Upsert(codex.ProviderID, accounts.Account{
		ID: "bad", AccountID: "acct-bad", Token: &oauth.Token{AccessToken: "tok-bad"}, Usage: staleUsage,
	}))
	require.NoError(t, accStore.Upsert(codex.ProviderID, accounts.Account{
		ID: "keyed", AccountID: "acct-keyed", APIKey: "sk-does-not-matter",
	}))

	fetch := func(_ context.Context, _, accessToken, _ string) (accounts.Usage, bool, error) {
		switch accessToken {
		case "tok-good":
			return accounts.Usage{Plan: "plus", Primary: accounts.UsageWindow{UsedPercent: 42, WindowMinutes: 60 * 24 * 7}}, true, nil
		case "tok-bad":
			return accounts.Usage{}, false, errors.New("boom")
		default:
			t.Fatalf("fetch called for unexpected token %q — api-key accounts must be skipped", accessToken)
			return accounts.Usage{}, false, nil
		}
	}

	got, err := RefreshAccountLimits(t.Context(), store, accStore, codex.ProviderID, fetch)
	require.NoError(t, err)
	require.Len(t, got, 3)

	byID := make(map[string]accounts.Account, len(got))
	for _, a := range got {
		byID[a.ID] = a
	}
	require.Equal(t, "plus", byID["good"].Usage.Plan, "a successful fetch must be persisted")
	require.Equal(t, 42, byID["good"].Usage.Primary.UsedPercent)
	require.Equal(t, staleUsage, byID["bad"].Usage, "a failed fetch must leave the stored snapshot untouched")
	require.False(t, byID["keyed"].Usage.Known(), "an api-key account has nothing to fetch")
}

// TestRefreshAccountLimits_NoUsageCapabilityIsNoOp covers a provider that
// doesn't report usage at all (accounts.CapabilitiesOf(...).Usage false):
// the accounts are returned unchanged and fetch is never called.
func TestRefreshAccountLimits_NoUsageCapabilityIsNoOp(t *testing.T) {
	t.Parallel()

	store := newRecordAccountStore(t)
	accStore := newTestAccountsStore(t)
	const providerID = "no-usage-provider"

	require.NoError(t, accStore.Upsert(providerID, accounts.Account{
		ID: "only", Token: &oauth.Token{AccessToken: "tok"},
	}))

	var calls atomic.Int32
	fetch := func(context.Context, string, string, string) (accounts.Usage, bool, error) {
		calls.Add(1)
		return accounts.Usage{}, false, nil
	}

	got, err := RefreshAccountLimits(t.Context(), store, accStore, providerID, fetch)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Zero(t, calls.Load(), "fetch must not be called for a provider with no usage capability")
}
