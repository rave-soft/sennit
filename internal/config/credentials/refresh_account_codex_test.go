package credentials

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	providerstate "github.com/rave-soft/sennit/internal/providers/state"
	"github.com/stretchr/testify/require"
)

// codexJWT is an unsigned token carrying the chatgpt_account_id claim
// codex.AccountID reads, mirroring internal/workspace/appws's fakeCodexJWT.
func codexJWT(t *testing.T, accountID string) string {
	t.Helper()
	claims := map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
		"exp":                         time.Now().Add(10 * 24 * time.Hour).Unix(),
	}
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

// writeCodexCLILogin plants a Codex CLI login for accountID in a CODEX_HOME
// of this test's own, which is what exchange's disk shortcut reads.
func writeCodexCLILogin(t *testing.T, accountID string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	accessToken := codexJWT(t, accountID)
	data, err := json.Marshal(map[string]any{
		"tokens": map[string]any{
			"access_token":  accessToken,
			"refresh_token": "rt-cli",
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, "auth.json"), data, 0o600))
	return accessToken
}

// newCodexAccountManager builds a Manager whose codex provider is live on
// activeAccessToken, with the real exchange in place: these tests are about
// what exchange itself decides, so the test hook must not stand in for it.
func newCodexAccountManager(t *testing.T, activeAccessToken string) (*Manager, *fakeStore) {
	t.Helper()

	live := &oauth.Token{
		AccessToken:  activeAccessToken,
		RefreshToken: "rt-live",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
	}
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set(codex.ProviderID, config.ProviderConfig{
		ID:         codex.ProviderID,
		Name:       "OpenAI Codex",
		APIKey:     live.AccessToken,
		OAuthToken: live,
	})
	runtimeProviders := csync.NewMap[string, providerstate.Provider]()
	runtimeProviders.Set(codex.ProviderID, providerstate.Provider{
		ID:         codex.ProviderID,
		Name:       "OpenAI Codex",
		APIKey:     live.AccessToken,
		OAuthToken: live,
		Account:    "acc-live",
	})

	configPath := filepath.Join(t.TempDir(), "sennit.json")
	require.NoError(t, os.WriteFile(configPath, []byte("{}"), 0o600))
	store := newFakeStore(
		&config.Config{Providers: providers, RuntimeProviders: runtimeProviders},
		configPath,
		filepath.Join(filepath.Dir(configPath), "locks"),
	)
	return New(store), store
}

// TestRefreshOAuthTokenForAccount_CodexDoesNotAdoptAnotherAccountsDiskToken
// is the regression guard for a cross-account leak: exchange may answer
// with the token the Codex CLI left on disk instead of spending a refresh
// token, and that disk token belongs to exactly one account. Matching it
// against the provider's *active* account meant that refreshing any other
// account, while the CLI happened to hold the active one's login, wrote the
// active account's token into the other account's entry — two entries
// authenticating as the same identity, with no sign anything was wrong.
//
// The refresh is expected to fail here: with the shortcut correctly
// declined, the only thing left to try is a real exchange, and the stored
// account deliberately carries no refresh token so that attempt fails
// offline. Failing is the right outcome — what matters is that the entry is
// left alone rather than overwritten with somebody else's credential.
func TestRefreshOAuthTokenForAccount_CodexDoesNotAdoptAnotherAccountsDiskToken(t *testing.T) {
	// No t.Parallel: writeCodexCLILogin pins CODEX_HOME for this test.
	diskAccessToken := writeCodexCLILogin(t, "acct-live")

	mgr, store := newCodexAccountManager(t, diskAccessToken)
	require.NoError(t, store.UpsertAccount(codex.ProviderID, accounts.Account{
		ID:        "acc-live",
		AccountID: "acct-live",
		Token:     &oauth.Token{AccessToken: diskAccessToken, RefreshToken: "rt-live"},
	}))
	require.NoError(t, store.UpsertAccount(codex.ProviderID, accounts.Account{
		ID:        "acc-other",
		AccountID: "acct-other",
		Token:     &oauth.Token{AccessToken: codexJWT(t, "acct-other")},
	}))

	err := mgr.RefreshOAuthTokenForAccount(context.Background(), config.ScopeGlobal, codex.ProviderID, "acc-other")
	require.Error(t, err, "the disk login belongs to another account, so there is nothing to adopt")

	accs, listErr := store.ListAccounts(codex.ProviderID)
	require.NoError(t, listErr)
	for _, a := range accs {
		if a.ID != "acc-other" {
			continue
		}
		require.NotEqual(t, diskAccessToken, a.Token.AccessToken,
			"the active account's token must never be persisted into another account")
		require.Equal(t, "acct-other", codex.AccountID(a.Token.AccessToken))
	}
}

// TestRefreshOAuthTokenForAccount_CodexAdoptsItsOwnDiskToken is the other
// half: the shortcut still applies when the disk login IS the account being
// refreshed, which is what keeps Sennit from spending the CLI's single-use
// refresh token and signing the CLI out.
func TestRefreshOAuthTokenForAccount_CodexAdoptsItsOwnDiskToken(t *testing.T) {
	// No t.Parallel: writeCodexCLILogin pins CODEX_HOME for this test.
	diskAccessToken := writeCodexCLILogin(t, "acct-live")

	mgr, store := newCodexAccountManager(t, diskAccessToken)
	require.NoError(t, store.UpsertAccount(codex.ProviderID, accounts.Account{
		ID:        "acc-live",
		AccountID: "acct-live",
		Token:     &oauth.Token{AccessToken: codexJWT(t, "acct-live"), RefreshToken: "rt-stale"},
	}))

	require.NoError(t, mgr.RefreshOAuthTokenForAccount(context.Background(), config.ScopeGlobal, codex.ProviderID, "acc-live"))

	accs, err := store.ListAccounts(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, accs, 1)
	require.Equal(t, diskAccessToken, accs[0].Token.AccessToken)
}
