package credentials

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/stretchr/testify/require"
)

// writeTokenToDisk persists token as the copilot provider credential in the
// fake config file at path, mimicking what another Sennit instance would
// leave behind after a successful refresh.
func writeTokenToDisk(t *testing.T, path string, token *oauth.Token) {
	t.Helper()
	configContent := fmt.Sprintf(`{
		"providers": {
			"copilot": {
				"api_key": %q,
				"oauth": {
					"access_token": %q,
					"refresh_token": %q,
					"expires_in": %d,
					"expires_at": %d
				}
			}
		}
	}`, token.AccessToken, token.AccessToken, token.RefreshToken, token.ExpiresIn, token.ExpiresAt)
	require.NoError(t, os.WriteFile(path, []byte(configContent), 0o600))
}

// newRefreshTestManager builds a Manager, backed by a fakeStore, whose
// copilot provider holds an expired OAuth token, persisted both in memory
// and on disk at configPath. Managers that share a configPath also share
// the per-provider refresh lock (RefreshLockPath derives from lockDir,
// shared across the fakeStores here), which lets a single test process
// faithfully simulate two Sennit instances: lock.File opens a fresh
// descriptor per call, so two managers block each other on the same lock
// file exactly as two processes would.
//
// The fixture uses "copilot" rather than a made-up provider ID for
// parity with the original ConfigStore-based test, though nothing here
// depends on the embedded catalog: the fake Store's PersistRefreshedToken
// always persists the credential fields regardless of catalog membership.
func newRefreshTestManager(t *testing.T, configPath string, exchange func(ctx context.Context, providerID, refreshToken string) (*oauth.Token, error)) *Manager {
	t.Helper()
	m, _ := newRefreshTestManagerWithStore(t, configPath, exchange)
	return m
}

// newRefreshTestManagerWithStore is newRefreshTestManager, but also returns
// the backing *fakeStore so a test can reach into it (e.g. to force
// PersistRefreshedToken to fail via persistFailures).
func newRefreshTestManagerWithStore(t *testing.T, configPath string, exchange func(ctx context.Context, providerID, refreshToken string) (*oauth.Token, error)) (*Manager, *fakeStore) {
	t.Helper()

	expired := &oauth.Token{
		AccessToken:  "at0",
		RefreshToken: "rt0",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
	}
	writeTokenToDisk(t, configPath, expired)

	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("copilot", config.ProviderConfig{
		ID:         "copilot",
		Name:       "GitHub Copilot",
		APIKey:     expired.AccessToken,
		OAuthToken: expired,
	})

	store := newFakeStore(&config.Config{Providers: providers}, configPath, filepath.Join(filepath.Dir(configPath), "locks"))
	m := New(store)
	m.exchangeToken = exchange
	return m, store
}

// TestRefreshOAuthToken_InProcessSingleFlight verifies that a storm of
// concurrent refresh calls for the same provider collapses into a single
// token exchange.
func TestRefreshOAuthToken_InProcessSingleFlight(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "sennit.json")

	var exchanges atomic.Int64
	mgr := newRefreshTestManager(t, configPath, func(ctx context.Context, providerID, refreshToken string) (*oauth.Token, error) {
		exchanges.Add(1)
		time.Sleep(50 * time.Millisecond) // hold the flight open so peers join
		return &oauth.Token{
			AccessToken:  "at1",
			RefreshToken: "rt1",
			ExpiresIn:    3600,
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		}, nil
	})

	const goroutines = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, goroutines)
	for range goroutines {
		wg.Go(func() {
			<-start
			errs <- mgr.RefreshOAuthToken(context.Background(), config.ScopeGlobal, "copilot")
		})
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int64(1), exchanges.Load(), "concurrent refreshes should collapse into one exchange")

	pc, ok := mgr.store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.Equal(t, "at1", pc.OAuthToken.AccessToken)
	require.Equal(t, "rt1", pc.OAuthToken.RefreshToken)
}

// TestRefreshOAuthToken_CrossProcessAdopt verifies that when two instances
// share a credential, only one performs the token exchange and the other
// adopts the rotated token from disk rather than reusing the consumed
// refresh token. The fake exchange models a rotating provider: reusing a
// refresh token it has already rotated returns an error, so a second
// exchange would be observable as a failure.
func TestRefreshOAuthToken_CrossProcessAdopt(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "sennit.json")

	var (
		mu          sync.Mutex
		current     = "rt0" // the only refresh token the server will accept
		exchanges   atomic.Int64
		reuseErrors atomic.Int64
	)
	exchange := func(ctx context.Context, providerID, refreshToken string) (*oauth.Token, error) {
		mu.Lock()
		defer mu.Unlock()
		if refreshToken != current {
			reuseErrors.Add(1)
			return nil, fmt.Errorf("refresh token revoked")
		}
		exchanges.Add(1)
		time.Sleep(50 * time.Millisecond) // hold the lock so the peer must wait
		current = "rt1"
		return &oauth.Token{
			AccessToken:  "at1",
			RefreshToken: "rt1",
			ExpiresIn:    3600,
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		}, nil
	}

	// Two managers sharing the same config file and refresh lock = two
	// "processes".
	a := newRefreshTestManager(t, configPath, exchange)
	b := newRefreshTestManager(t, configPath, exchange)

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, m := range []*Manager{a, b} {
		wg.Go(func() {
			<-start
			errs <- m.RefreshOAuthToken(context.Background(), config.ScopeGlobal, "copilot")
		})
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	require.Equal(t, int64(1), exchanges.Load(), "only one instance should exchange")
	require.Equal(t, int64(0), reuseErrors.Load(), "no instance should reuse a rotated refresh token")

	// Both instances converge on the rotated token.
	for name, m := range map[string]*Manager{"a": a, "b": b} {
		pc, ok := m.store.Config().Providers.Get("copilot")
		require.True(t, ok, name)
		require.Equal(t, "at1", pc.OAuthToken.AccessToken, name)
		require.Equal(t, "rt1", pc.OAuthToken.RefreshToken, name)
	}
}

// rotatingExchange models a provider that rotates refresh tokens and
// revokes the previous one: presenting anything other than the currently
// live refresh token fails the way a real reuse-detecting server would.
// Tokens are handed out as at<n>/rt<n> starting at next. The returned
// counters report successful exchanges and reuse attempts.
func rotatingExchange(live string, next int) (exchange func(ctx context.Context, providerID, refreshToken string) (*oauth.Token, error), exchanges, reuse *atomic.Int64) {
	var (
		mu        sync.Mutex
		exchanged atomic.Int64
		reused    atomic.Int64
	)
	return func(ctx context.Context, providerID, refreshToken string) (*oauth.Token, error) {
		mu.Lock()
		defer mu.Unlock()
		if refreshToken != live {
			reused.Add(1)
			return nil, &oauth.TokenExchangeError{StatusCode: 400, Body: `{"error":"invalid_grant"}`}
		}
		exchanged.Add(1)
		token := &oauth.Token{
			AccessToken:  fmt.Sprintf("at%d", next),
			RefreshToken: fmt.Sprintf("rt%d", next),
			ExpiresIn:    3600,
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		}
		live = token.RefreshToken
		next++
		return token, nil
	}, &exchanged, &reused
}

// TestRefreshOAuthToken_StalePeerBorrowsRotatedRefreshToken covers the
// instance that has been idle while a peer rotated the credential several
// times. Its own refresh token is long dead, and the token on disk has
// itself aged out, so there is nothing to adopt outright. The stale
// instance must still recover by exchanging with the refresh token from
// disk rather than presenting its own revoked one, which would revoke the
// whole token family and force the user to log in again.
func TestRefreshOAuthToken_StalePeerBorrowsRotatedRefreshToken(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "sennit.json")
	exchange, exchanges, reuse := rotatingExchange("rt3", 4)
	mgr := newRefreshTestManager(t, configPath, exchange)

	// Disk holds the peer's third rotation, whose access token has also
	// expired. In memory we are still back on the original credential.
	writeTokenToDisk(t, configPath, &oauth.Token{
		AccessToken:  "at3",
		RefreshToken: "rt3",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
	})

	require.NoError(t, mgr.RefreshOAuthToken(context.Background(), config.ScopeGlobal, "copilot"))
	require.Equal(t, int64(1), exchanges.Load())
	require.Equal(t, int64(0), reuse.Load(), "must not present its own revoked refresh token")

	pc, ok := mgr.store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.Equal(t, "at4", pc.OAuthToken.AccessToken)
	require.Equal(t, "rt4", pc.OAuthToken.RefreshToken)
	require.Equal(t, "at4", pc.APIKey)
}

// TestRefreshOAuthToken_AdoptsFresherDiskToken verifies that an instance
// whose in-memory credential has aged out adopts a peer's still-valid
// token from disk without spending an exchange at all.
func TestRefreshOAuthToken_AdoptsFresherDiskToken(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "sennit.json")
	exchange, exchanges, _ := rotatingExchange("rt9", 10)
	mgr := newRefreshTestManager(t, configPath, exchange)

	writeTokenToDisk(t, configPath, &oauth.Token{
		AccessToken:  "at9",
		RefreshToken: "rt9",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	})

	require.NoError(t, mgr.RefreshOAuthToken(context.Background(), config.ScopeGlobal, "copilot"))
	require.Equal(t, int64(0), exchanges.Load(), "a usable peer token needs no exchange")

	pc, ok := mgr.store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.Equal(t, "at9", pc.OAuthToken.AccessToken)
	require.Equal(t, "at9", pc.APIKey)
}

// TestRefreshOAuthToken_IgnoresOlderDiskToken guards against walking
// backwards: a config file holding an older credential than the one we
// already have must not be adopted or borrowed from.
func TestRefreshOAuthToken_IgnoresOlderDiskToken(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "sennit.json")
	exchange, exchanges, reuse := rotatingExchange("rt0", 1)
	mgr := newRefreshTestManager(t, configPath, exchange)

	writeTokenToDisk(t, configPath, &oauth.Token{
		AccessToken:  "ancient",
		RefreshToken: "ancient-rt",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-24 * time.Hour).Unix(),
	})

	require.NoError(t, mgr.RefreshOAuthToken(context.Background(), config.ScopeGlobal, "copilot"))
	require.Equal(t, int64(1), exchanges.Load())
	require.Equal(t, int64(0), reuse.Load())

	pc, ok := mgr.store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.Equal(t, "rt1", pc.OAuthToken.RefreshToken)
}

// TestRefreshOAuthToken_RetriesPersistBeforeGivingUp verifies that a
// transient PersistRefreshedToken failure is retried (refreshPersistAttempts
// times) rather than giving up on the first error.
func TestRefreshOAuthToken_RetriesPersistBeforeGivingUp(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "sennit.json")
	exchange, exchanges, _ := rotatingExchange("rt0", 1)
	mgr, store := newRefreshTestManagerWithStore(t, configPath, exchange)

	// Fail every persist attempt but the last, so the call only succeeds
	// because of the retry loop.
	store.persistFailures = refreshPersistAttempts - 1

	require.NoError(t, mgr.RefreshOAuthToken(context.Background(), config.ScopeGlobal, "copilot"))
	require.Equal(t, int64(1), exchanges.Load(), "a persist failure must not trigger a second exchange")
	require.Equal(t, refreshPersistAttempts, store.persistCallCount)

	pc, ok := mgr.store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.Equal(t, "rt1", pc.OAuthToken.RefreshToken, "the refreshed token must still be published once persist eventually succeeds")
}

// TestRefreshOAuthToken_PublishesTokenEvenIfPersistNeverSucceeds verifies
// that, after retries are exhausted, refreshOAuthTokenLocked still
// publishes the refreshed token in memory (there is no way to undo an
// already-spent refresh token) and surfaces the persist failure to the
// caller rather than swallowing it.
func TestRefreshOAuthToken_PublishesTokenEvenIfPersistNeverSucceeds(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "sennit.json")
	exchange, exchanges, _ := rotatingExchange("rt0", 1)
	mgr, store := newRefreshTestManagerWithStore(t, configPath, exchange)

	// Every attempt fails; retries alone cannot save this refresh.
	store.persistFailures = refreshPersistAttempts + 10

	err := mgr.RefreshOAuthToken(context.Background(), config.ScopeGlobal, "copilot")
	require.ErrorIs(t, err, errPersistFailed)
	require.Equal(t, int64(1), exchanges.Load())
	require.Equal(t, refreshPersistAttempts, store.persistCallCount, "must not retry forever")

	// The exchange already spent the old refresh token, so the refreshed
	// one must still be published in memory even though disk was never
	// updated -- silently discarding it here would leave the process
	// unable to refresh again with the (now stale) token still on disk.
	pc, ok := mgr.store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.Equal(t, "rt1", pc.OAuthToken.RefreshToken)
}
