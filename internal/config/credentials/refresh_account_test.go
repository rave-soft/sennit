package credentials

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/stretchr/testify/require"
)

// setActiveAccount marks accountID as the provider's live account, the way
// ActivateAccount does, so RefreshOAuthTokenForAccount can tell the active
// account apart from a merely stored one.
func setActiveAccount(t *testing.T, store *fakeStore, providerID, accountID string) {
	t.Helper()
	rp, ok := store.Config().RuntimeProvider(providerID)
	require.True(t, ok)
	rp.Account = accountID
	store.Config().SetRuntimeProvider(providerID, rp)
}

func freshToken(access, refresh string) *oauth.Token {
	return &oauth.Token{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}
}

// TestRefreshOAuthTokenForAccount_StoredAccount covers the ordinary case:
// an account that is not live is exchanged against its own refresh token
// and the result lands in the account store, without disturbing which
// account is active or what the live credential holds.
func TestRefreshOAuthTokenForAccount_StoredAccount(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "sennit.json")

	var presented atomic.Value
	mgr, store := newRefreshTestManagerWithStore(t, configPath, func(_ context.Context, _, _, refreshToken string) (*oauth.Token, error) {
		presented.Store(refreshToken)
		return freshToken("at-stored", "rt-stored-new"), nil
	})
	setActiveAccount(t, store, "copilot", "live")
	require.NoError(t, store.UpsertAccount("copilot", accounts.Account{ID: "live", Token: freshToken("at-live", "rt-live")}))
	require.NoError(t, store.UpsertAccount("copilot", accounts.Account{ID: "spare", Token: freshToken("at-spare", "rt-spare")}))

	require.NoError(t, mgr.RefreshOAuthTokenForAccount(context.Background(), config.ScopeGlobal, "copilot", "spare"))

	require.Equal(t, "rt-spare", presented.Load(), "the stored account's own refresh token is what gets spent")

	accs, err := store.ListAccounts("copilot")
	require.NoError(t, err)
	byID := map[string]accounts.Account{}
	for _, a := range accs {
		byID[a.ID] = a
	}
	require.Equal(t, "at-stored", byID["spare"].Token.AccessToken)
	require.Equal(t, "at-live", byID["live"].Token.AccessToken, "refreshing a stored account must not touch another one")

	rp, ok := store.Config().RuntimeProvider("copilot")
	require.True(t, ok)
	require.Equal(t, "live", rp.Account, "refreshing a stored account must not make it live")
	require.NotEqual(t, "at-stored", rp.OAuthToken.AccessToken, "a stored account's new token must not be published as the live credential")
}

// TestRefreshOAuthTokenForAccount_ActiveAccount is the regression guard for
// the hazard the per-account path would otherwise carry: the active
// account's token lives both in the account store and in the live
// credential, and an exchange that updates only the store leaves the live
// credential holding a refresh token the provider has just spent - so the
// next ordinary refresh fails with invalid_grant. Both copies must end up
// on the new token.
func TestRefreshOAuthTokenForAccount_ActiveAccount(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "sennit.json")

	var exchanges atomic.Int64
	mgr, store := newRefreshTestManagerWithStore(t, configPath, func(_ context.Context, _, _, _ string) (*oauth.Token, error) {
		exchanges.Add(1)
		return freshToken("at-new", "rt-new"), nil
	})
	setActiveAccount(t, store, "copilot", "live")
	require.NoError(t, store.UpsertAccount("copilot", accounts.Account{ID: "live", Token: freshToken("at-live", "rt-live")}))

	require.NoError(t, mgr.RefreshOAuthTokenForAccount(context.Background(), config.ScopeGlobal, "copilot", "live"))
	require.Equal(t, int64(1), exchanges.Load())

	rp, ok := store.Config().RuntimeProvider("copilot")
	require.True(t, ok)
	require.Equal(t, "at-new", rp.OAuthToken.AccessToken, "the live credential must carry the refreshed token")
	require.Equal(t, "rt-new", rp.OAuthToken.RefreshToken)

	accs, err := store.ListAccounts("copilot")
	require.NoError(t, err)
	require.Len(t, accs, 1)
	require.Equal(t, "rt-new", accs[0].Token.RefreshToken,
		"the account store's copy must not be left holding the refresh token the exchange just spent")
}

// TestRefreshOAuthTokenForAccount_UnknownAccount fails rather than
// silently exchanging against the wrong credential.
func TestRefreshOAuthTokenForAccount_UnknownAccount(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "sennit.json")

	mgr, store := newRefreshTestManagerWithStore(t, configPath, func(_ context.Context, _, _, _ string) (*oauth.Token, error) {
		t.Fatal("no exchange should be attempted for an unknown account")
		return nil, nil
	})
	setActiveAccount(t, store, "copilot", "live")

	err := mgr.RefreshOAuthTokenForAccount(context.Background(), config.ScopeGlobal, "copilot", "ghost")
	require.ErrorContains(t, err, "not found")
}
