package config

import (
	"testing"

	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/stretchr/testify/require"
)

func TestAccountsService_ListMigratesLegacyCredential(t *testing.T) {
	t.Parallel()

	store := newRecordAccountStore(t)
	token := fakeCodexJWT(t, "acct-existing")
	provider, _ := store.Config().Providers.Get(codex.ProviderID)
	provider.APIKey = token
	provider.OAuthToken = &oauth.Token{AccessToken: token}
	store.Config().Providers.Set(codex.ProviderID, provider)

	service := NewAccountsService(store, newTestAccountsStore(t), nil)
	listed, err := service.List(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, "acct-existing", listed[0].AccountID)
}

func TestAccountsService_ActivateMissingAccountPreservesError(t *testing.T) {
	t.Parallel()

	service := NewAccountsService(newRecordAccountStore(t), newTestAccountsStore(t), nil)
	err := service.Activate(ScopeGlobal, codex.ProviderID, "missing")
	require.EqualError(t, err, "account missing not found for provider codex")
}
