// Package credentials owns the OAuth token lifecycle for provider
// credentials: refreshing tokens (with cross-process locking and
// in-process single-flighting), signalling interactive re-authentication
// completion, and importing a GitHub Copilot token found on disk.
//
// It was split out of config.ConfigStore, which had grown to own four
// unrelated contexts at once.
// The dependency runs one way: credentials imports config, not the other
// way around. Manager reaches back into ConfigStore only through the
// narrow Store interface below, so config never has to import
// credentials to keep ConfigStore's own methods working.
//
// Exactly one Manager must exist per process. SignalAuthComplete (called
// from internal/workspace) and WaitForTokenChange (awaited from
// internal/agent) communicate through Manager's authSignals map; two
// Manager instances layered over the same Store would let a signal fired
// on one instance go unseen by a waiter registered on the other, which
// blocks until its timeout instead of resuming. See internal/app/app.go,
// which constructs the single instance and hands it to every consumer.
package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/lock"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/oauth/copilot"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/singleflight"
)

// refreshLockDeadline bounds how long RefreshOAuthToken waits for the
// per-provider cross-process refresh lock. It must exceed the token
// exchange HTTP timeout (30s) so that a peer mid-exchange is given time
// to finish and publish its result, which we then adopt instead of
// running our own exchange. Running our own would reuse an
// already-rotated refresh token and trip the provider's reuse detection,
// revoking the whole token family.
const refreshLockDeadline = 45 * time.Second

// Store is the narrow view of config.ConfigStore that Manager needs. It
// exists so credentials never has to import the concrete ConfigStore
// type, which would round-trip back into config and reintroduce the
// cycle this split exists to avoid.
type Store interface {
	// Config returns the current pure-data config.
	Config() *config.Config
	// ConfigPath returns the on-disk config file path for scope.
	ConfigPath(scope config.Scope) (string, error)
	// RefreshLockPath returns the path to the per-provider cross-process
	// refresh lock file. Owned by ConfigStore (see its doc comment)
	// because withRefreshLock, on the store side, needs the same path.
	RefreshLockPath(providerID string) string
	// HasConfigField checks whether a key exists in the config file.
	HasConfigField(scope config.Scope, key string) bool
	// UpdateProviderCredentials publishes a credential-only provider
	// update in memory; see ConfigStore.UpdateProviderCredentials.
	UpdateProviderCredentials(providerID, apiKey string, token *oauth.Token) error
	// SetProviderAPIKey sets and persists a provider's credential; see
	// ConfigStore.SetProviderAPIKey.
	SetProviderAPIKey(scope config.Scope, providerID string, apiKey any) error
	// PersistRefreshedToken writes a refreshed token's credential fields
	// to disk and republishes the in-memory config; see
	// ConfigStore.PersistRefreshedToken.
	PersistRefreshedToken(scope config.Scope, providerID string, cfg config.ProviderConfig, token *oauth.Token) error
}

// Manager owns OAuth token refresh, cross-instance auth-completion
// signalling, and Copilot token import for one process. Exactly one
// instance must exist per process — see the package doc.
type Manager struct {
	store Store

	// refreshSF collapses concurrent in-process OAuth refreshes for the
	// same provider into a single attempt. Combined with the per-provider
	// cross-process refresh lock, it ensures only one token exchange runs
	// at a time. See RefreshOAuthToken.
	refreshSF singleflight.Group

	// exchangeToken performs the provider-specific OAuth token exchange.
	// It is a field so tests can substitute a fake exchange without making
	// real network calls. Production code leaves it nil, and exchange falls
	// back to the real provider clients.
	exchangeToken func(ctx context.Context, providerID, refreshToken string) (*oauth.Token, error)

	// authSignalMu guards authSignals, which maps provider IDs to
	// channels that WaitForTokenChange blocks on. SignalAuthComplete
	// closes the channel to unblock waiters; a new channel is created
	// on the next wait.
	authSignalMu sync.Mutex
	authSignals  map[string]chan struct{}
}

// Option configures a Manager at construction time.
type Option func(*Manager)

// WithExchangeToken overrides the OAuth token exchange function used by
// RefreshOAuthToken. It exists so tests outside this package can drive a
// Manager through a real refresh without making a network call; production
// callers must not pass this and should leave exchange on the real
// provider clients (see exchange).
func WithExchangeToken(exchange func(ctx context.Context, providerID, refreshToken string) (*oauth.Token, error)) Option {
	return func(m *Manager) { m.exchangeToken = exchange }
}

// New constructs a Manager backed by store. Callers must construct
// exactly one Manager per process and share it with every consumer — see
// the package doc.
func New(store Store, opts ...Option) *Manager {
	m := &Manager{store: store}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// RefreshOAuthToken refreshes the OAuth token for the given provider.
//
// Providers like Hyper rotate refresh tokens: each exchange consumes the
// caller's refresh token, issues a new pair, and revokes the old one. If
// two sennit instances (or two goroutines) refresh concurrently with the
// same stored refresh token, the second exchange reuses an already-revoked
// token, trips the provider's reuse detection, and revokes the entire
// token family — leaving both with dead tokens even though each refresh
// "succeeded".
//
// To prevent that, refreshes are single-flighted at two levels:
//
//   - In-process: refreshSF collapses concurrent goroutines for the same
//     provider into one attempt.
//   - Cross-process: a per-provider advisory lock is held across the whole
//     read-decide-exchange-write cycle, so only one process exchanges at a
//     time. A process that acquires the lock after a peer rotated finds the
//     peer's fresh token on disk and adopts it instead of exchanging.
func (m *Manager) RefreshOAuthToken(ctx context.Context, scope config.Scope, providerID string) error {
	key := fmt.Sprintf("%d\x00%s", scope, providerID)
	_, err, _ := m.refreshSF.Do(key, func() (any, error) {
		return nil, m.refreshOAuthTokenLocked(ctx, scope, providerID)
	})
	return err
}

// refreshOAuthTokenLocked performs the cross-process single-flighted
// refresh. It is invoked through refreshSF, so at most one goroutine per
// provider runs it at a time within this process.
func (m *Manager) refreshOAuthTokenLocked(ctx context.Context, scope config.Scope, providerID string) error {
	cfg := m.store.Config()
	providerConfig, exists := cfg.Providers.Get(providerID)
	if !exists {
		return fmt.Errorf("provider %s not found", providerID)
	}
	if providerConfig.OAuthToken == nil {
		return fmt.Errorf("provider %s does not have an OAuth token", providerID)
	}
	entryToken := providerConfig.OAuthToken

	// Acquire the per-provider cross-process refresh lock. This is a
	// dedicated lock file, not the config-write lock, and it does not take
	// s.mu — so the network exchange below cannot stall unrelated config
	// operations. The deadline exceeds the exchange timeout so that a peer
	// mid-exchange has time to publish a token we can adopt. Lock ordering:
	// the refresh lock is always taken before the config-write lock (via
	// update, which takes writeMu then s.mu — the same nesting order
	// configureProviders already uses when it calls RemoveConfigField
	// during a reload), never the reverse, so no deadlock is possible.
	lockCtx, cancel := context.WithTimeout(ctx, refreshLockDeadline)
	defer cancel()
	release, lockErr := lock.File(lockCtx, m.store.RefreshLockPath(providerID))
	if lockErr != nil {
		// Could not acquire the lock (peer wedged or deadline hit). Prefer a
		// usable token already on disk over forcing our own exchange, which
		// would risk reusing a rotated refresh token.
		if diskToken := m.usableDiskToken(scope, providerID, entryToken); diskToken != nil {
			slog.Warn("Refresh lock unavailable; adopting token from disk", "provider", providerID, "error", lockErr)
			m.applyToken(providerConfig, diskToken, providerID)
			return nil
		}
		return fmt.Errorf("acquire refresh lock for provider %s: %w", providerID, lockErr)
	}
	defer release()

	// Now that we hold the lock, disk is the authority on which credential
	// is current: a peer may have rotated ours away while we waited. Adopt
	// a newer token outright when it is still usable, and otherwise switch
	// to its refresh token for the exchange below. Presenting our own
	// already-rotated refresh token would trip the provider's reuse
	// detection and revoke the whole family, forcing an interactive login.
	if diskToken := m.newerDiskToken(scope, providerID, entryToken); diskToken != nil {
		if !diskToken.IsExpired() {
			slog.Info("Adopting token refreshed by another session", "provider", providerID)
			m.applyToken(providerConfig, diskToken, providerID)
			return nil
		}
		slog.Info("Exchanging with refresh token rotated by another session", "provider", providerID)
		entryToken = diskToken
	}

	// Disk still holds our token (or no newer peer token exists) and we hold
	// the lock, so we are the sole exchanger. Perform the exchange.
	refreshedToken, refreshErr := m.exchange(ctx, providerID, entryToken.RefreshToken)
	if refreshErr != nil {
		// The exchange may have failed because a peer rotated the refresh
		// token in a window we did not cover. Re-check disk: adopt a usable
		// token, or retry once with the peer's newer refresh token.
		if diskToken := m.newerDiskToken(scope, providerID, entryToken); diskToken != nil {
			if !diskToken.IsExpired() {
				slog.Info("Adopting token refreshed by another session after exchange failure", "provider", providerID)
				m.applyToken(providerConfig, diskToken, providerID)
				return nil
			}
			slog.Info("Retrying exchange with refresh token rotated by another session", "provider", providerID)
			refreshedToken, refreshErr = m.exchange(ctx, providerID, diskToken.RefreshToken)
		}
	}
	if refreshErr != nil {
		return fmt.Errorf("failed to refresh OAuth token for provider %s: %w", providerID, refreshErr)
	}

	slog.Info("Successfully refreshed OAuth token", "provider", providerID)
	m.applyToken(providerConfig, refreshedToken, providerID)

	if err := m.store.PersistRefreshedToken(scope, providerID, providerConfig, refreshedToken); err != nil {
		return err
	}
	return nil
}

// WaitForTokenChange blocks until SignalAuthComplete is called for the
// given provider or the context is cancelled. It is used by OnAuthRefresh
// callbacks to wait for interactive re-authentication to complete before
// retrying a failed request. The channel is created atomically with the
// wait registration so a concurrent SignalAuthComplete cannot miss it.
func (m *Manager) WaitForTokenChange(ctx context.Context, providerID string) error {
	m.authSignalMu.Lock()
	ch, ok := m.authSignals[providerID]
	if !ok {
		ch = make(chan struct{})
		if m.authSignals == nil {
			m.authSignals = make(map[string]chan struct{})
		}
		m.authSignals[providerID] = ch
	}
	m.authSignalMu.Unlock()

	select {
	case <-ch:
		// Remove the consumed signal so a subsequent
		// SignalAuthComplete does not close an already-closed
		// channel.
		m.authSignalMu.Lock()
		if m.authSignals[providerID] == ch {
			delete(m.authSignals, providerID)
		}
		m.authSignalMu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SignalAuthComplete unblocks any goroutine waiting in WaitForTokenChange
// for the given provider. If no waiter exists yet, it pre-creates and
// immediately closes the channel so a subsequent WaitForTokenChange
// returns without blocking. This eliminates the race where the signal
// fires before the waiter registers.
func (m *Manager) SignalAuthComplete(providerID string) {
	m.authSignalMu.Lock()
	defer m.authSignalMu.Unlock()
	if ch, ok := m.authSignals[providerID]; ok {
		delete(m.authSignals, providerID)
		select {
		case <-ch:
			// Already closed by a previous signal; nothing to do.
		default:
			close(ch)
		}
	} else {
		// No waiter yet. Pre-create a closed channel so the next
		// WaitForTokenChange returns immediately.
		if m.authSignals == nil {
			m.authSignals = make(map[string]chan struct{})
		}
		ch := make(chan struct{})
		close(ch)
		m.authSignals[providerID] = ch
	}
}

// newerDiskToken returns the on-disk token for the provider when it is
// newer than entryToken — i.e. another session (possibly in another
// process) has already rotated the credential. It returns nil when disk
// holds nothing newer than what we started with.
//
// Newness is judged by expiry as well as identity, so a config file that
// somehow holds an older token cannot drag us backwards. The result may
// itself be expired: providers that rotate refresh tokens invalidate ours
// the moment a peer refreshes, so the peer's refresh token is the only one
// the provider will still accept even after its access token ages out.
// Callers decide whether to adopt the token wholesale or merely borrow its
// refresh token.
func (m *Manager) newerDiskToken(scope config.Scope, providerID string, entryToken *oauth.Token) *oauth.Token {
	diskToken, err := m.loadTokenFromDisk(scope, providerID)
	if err != nil {
		slog.Warn("Failed to read token from config file", "provider", providerID, "error", err)
		return nil
	}
	if diskToken == nil {
		return nil
	}
	if diskToken.AccessToken == entryToken.AccessToken {
		// Same token we started with; nobody rotated since.
		return nil
	}
	if diskToken.RefreshToken == "" && entryToken.RefreshToken != "" {
		// Adopting would thread us with no way to refresh later, and
		// there is nothing to borrow for an exchange.
		return nil
	}
	if diskToken.ExpiresAt < entryToken.ExpiresAt {
		// Older than ours; nothing to gain from adopting it.
		return nil
	}
	return diskToken
}

// usableDiskToken returns the on-disk token only when it is both newer
// than entryToken and still valid, meaning it can be adopted as-is with
// no exchange at all.
func (m *Manager) usableDiskToken(scope config.Scope, providerID string, entryToken *oauth.Token) *oauth.Token {
	diskToken := m.newerDiskToken(scope, providerID, entryToken)
	if diskToken == nil || diskToken.IsExpired() {
		return nil
	}
	return diskToken
}

// exchange performs the provider-specific OAuth token exchange. Tests may
// override it via the exchangeToken field; production uses the real
// provider clients.
func (m *Manager) exchange(ctx context.Context, providerID, refreshToken string) (*oauth.Token, error) {
	if m.exchangeToken != nil {
		return m.exchangeToken(ctx, providerID, refreshToken)
	}
	switch providerID {
	case string(catwalk.InferenceProviderCopilot):
		return copilot.RefreshToken(ctx, refreshToken)
	case codex.ProviderID:
		// An imported login shares the Codex CLI's refresh token, and that
		// token is single-use: spending it here logs the CLI out. The CLI
		// refreshes on its own schedule, so adopt a newer token it has
		// already produced for this account rather than taking its last
		// one.
		if token, ok := codex.TokenFromDiskFor(m.providerAccount(providerID)); ok {
			slog.Debug("Adopted a Codex token from the CLI instead of spending the refresh token")
			return token, nil
		}
		// Codex is reachable only through a proxy for some users, and a
		// refresh that ignored the one the provider is configured with
		// would fail while every model call kept working.
		return codex.RefreshToken(ctx, m.providerProxy(providerID), refreshToken)
	default:
		return nil, fmt.Errorf("OAuth refresh not supported for provider %s", providerID)
	}
}

// providerAccount is the account the provider's current credential belongs
// to, or "" when there is none to read.
func (m *Manager) providerAccount(providerID string) string {
	cfg := m.store.Config()
	if cfg == nil {
		return ""
	}
	pc, ok := cfg.Providers.Get(providerID)
	if !ok {
		return ""
	}
	return codex.AccountID(pc.APIKey)
}

// providerProxy returns the proxy configured for a provider, or "" when it
// has none. A missing provider is not an error here: the refresh path can
// run while the entry is mid-rewrite, and no proxy is the right default.
func (m *Manager) providerProxy(providerID string) string {
	cfg := m.store.Config()
	if cfg == nil {
		return ""
	}
	pc, ok := cfg.Providers.Get(providerID)
	if !ok {
		return ""
	}
	return pc.ProxyURL
}

// applyToken updates the in-memory provider config with the given token.
func (m *Manager) applyToken(_ config.ProviderConfig, token *oauth.Token, providerID string) {
	// Keep all credential publication versioned so consumers can invalidate
	// compiled providers without changing the selected model identity.
	if err := m.store.UpdateProviderCredentials(providerID, token.AccessToken, token); err != nil {
		slog.Error("Failed to publish refreshed provider credentials", "provider", providerID, "error", err)
	}
}

// loadTokenFromDisk reads the OAuth token for the given provider from the
// config file on disk. Returns nil if the token is not found or matches the
// current in-memory token.
func (m *Manager) loadTokenFromDisk(scope config.Scope, providerID string) (*oauth.Token, error) {
	path, err := m.store.ConfigPath(scope)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	oauthKey := config.ProviderFieldKey(providerID, "oauth")
	oauthResult := gjson.Get(string(data), oauthKey)
	if !oauthResult.Exists() {
		return nil, nil
	}

	var token oauth.Token
	if err := json.Unmarshal([]byte(oauthResult.Raw), &token); err != nil {
		return nil, err
	}

	if token.AccessToken == "" {
		return nil, nil
	}

	return &token, nil
}

// ImportCopilot attempts to import a GitHub Copilot token from disk.
func (m *Manager) ImportCopilot() (*oauth.Token, bool) {
	if m.store.HasConfigField(config.ScopeGlobal, "providers.copilot.api_key") || m.store.HasConfigField(config.ScopeGlobal, "providers.copilot.oauth") {
		return nil, false
	}

	diskToken, hasDiskToken := copilot.RefreshTokenFromDisk()
	if !hasDiskToken {
		return nil, false
	}

	slog.Info("Found existing GitHub Copilot token on disk. Authenticating...")
	token, err := copilot.RefreshToken(context.TODO(), diskToken)
	if err != nil {
		slog.Error("Unable to import GitHub Copilot token", "error", err)
		return nil, false
	}

	// SetProviderAPIKey both applies the token in memory and persists it,
	// so a second explicit write of the same keys is unnecessary.
	if err := m.store.SetProviderAPIKey(config.ScopeGlobal, string(catwalk.InferenceProviderCopilot), token); err != nil {
		slog.Error("Unable to save GitHub Copilot token to disk", "error", err)
		return token, false
	}

	slog.Info("GitHub Copilot successfully imported")
	return token, true
}
