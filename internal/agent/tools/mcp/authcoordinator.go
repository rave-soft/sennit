package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	mcpoauth "github.com/rave-soft/sennit/internal/oauth/mcp"
)

// authCoordinator owns the OAuth/auth flow: the explicit BeginAuth /
// AuthenticateMCP entry points, the in-flight authFlow bookkeeping, and
// publishing/detaching the OAuth handler a session's transport uses. Like
// connectionManager, it holds no lock of its own over ownership state; it
// reaches through reg for every owns-check-then-commit (updateStateFor,
// publishMu-guarded detach, connectAndRegister, ...), so publishMu is only
// ever acquired by a Registry-owned method and the publishMu -> catalogMu
// order can't be entered in the other direction from here.
type authCoordinator struct {
	reg *Registry

	authMu    sync.Mutex
	authFlows map[string]*authFlow

	runAuth func(ctx context.Context, cfg ConfigProvider, name string, m config.MCPConfig, owner attemptID) error
}

// setAuthTerminal settles an auth attempt on error: a token/consent problem
// goes to StateNeedsAuth (recoverable by re-authenticating), anything else
// goes to StateError.
//
// A bare context.Canceled/DeadlineExceeded on err is not enough to call this
// recoverable: createSession derives its connect context from
// context.WithoutCancel(ctx) plus its own mcpTimeout-bound cancellation (see
// the comment there), so a server that simply never answers surfaces the
// exact same sentinels as a real user cancel, without ctx itself ever being
// touched. ctx.Err() is the caller's own context, so it is only non-nil when
// the caller genuinely gave up — e.g. publishSession later using ctx for
// getTools/getPrompts/getResources after a connect the caller had already
// abandoned. errInteractiveAuthTimeout is the other legitimate recoverable
// case: transport.go tags it explicitly when interactiveAuthTimeout (the
// user never finished the browser login) is what expired, so it doesn't
// depend on ctx at all. Anything else - including a connect that merely ran
// past its own mcpTimeout - is a genuine failure and gets StateError.
func (ac *authCoordinator) setAuthTerminal(ctx context.Context, name string, owner attemptID, err error) {
	if err == nil {
		return
	}
	if ctx.Err() != nil || errors.Is(err, errInteractiveAuthTimeout) || isOAuthInitErr(err) {
		ac.reg.updateStateFor(name, owner, StateNeedsAuth, nil)
		return
	}
	ac.reg.updateStateFor(name, owner, StateError, err)
}

// MCPAuthURL returns the current OAuth authorization URL for the named
// MCP, or empty if none is in progress.
func (ac *authCoordinator) MCPAuthURL(name string) string {
	ac.reg.publishMu.Lock()
	defer ac.reg.publishMu.Unlock()
	publication, ok := ac.reg.authURLs.Get(name)
	if !ok || publication.auth == nil || publication.auth.handler == nil || publication.gen != ac.reg.currentGen(name) {
		return ""
	}
	return publication.auth.handler.AuthURL()
}

// PendingAuthMCPs returns MCP servers in StateNeedsAuth with their URLs.
func (ac *authCoordinator) PendingAuthMCPs(cfg ConfigProvider) []PendingAuthServer {
	var pending []PendingAuthServer
	for name, info := range ac.reg.states.Seq2() {
		if info.State == StateNeedsAuth {
			url := ""
			if m, ok := cfg.Config().MCP[name]; ok {
				url = m.URL
			}
			pending = append(pending, PendingAuthServer{Name: name, URL: url})
		}
	}
	slices.SortFunc(pending, func(a, b PendingAuthServer) int {
		return strings.Compare(a.Name, b.Name)
	})
	return pending
}

// BeginAuth starts the OAuth flow for a server in StateNeedsAuth but
// suppresses opening a local browser; the caller is responsible for
// surfacing the authorization URL (via [MCPAuthURL]) to the user. It returns
// a finish function that must be called exactly once with the request
// context: finish blocks until the flow completes and returns the result.
//
// Only one browser-suppressed flow per server may be in progress. The
// returned cancel function aborts the flow without waiting; use it when the
// caller's context is cancelled.
func (ac *authCoordinator) BeginAuth(cfg ConfigProvider, name string) (finish func(ctx context.Context) error, cancel context.CancelFunc, err error) {
	m, exists := cfg.Config().MCP[name]
	if !exists {
		return nil, nil, fmt.Errorf("mcp '%s' not found in configuration", name)
	}
	if !usesOAuth(m) {
		return nil, nil, fmt.Errorf("mcp '%s' does not use OAuth authentication", name)
	}
	lock := ac.suppressLock(name)
	if !lock.TryLock() {
		return nil, nil, fmt.Errorf("mcp '%s' already has an authentication in progress", name)
	}
	ctx, flowCancel := context.WithCancel(context.Background())
	ctx = mcpoauth.WithInteractive(context.WithValue(ctx, suppressBrowserKey{}, true))
	owner, ownerErr := ac.reg.beginAttempt(name)
	if ownerErr != nil {
		flowCancel()
		lock.Unlock()
		return nil, nil, ownerErr
	}
	flow := &authFlow{
		cancel:     flowCancel,
		done:       make(chan struct{}),
		workerDone: make(chan struct{}),
		owner:      owner,
		lock:       lock,
	}
	ac.authMu.Lock()
	ac.authFlows[name] = flow
	ac.authMu.Unlock()
	go func() {
		var workerErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				workerErr = fmt.Errorf("panic in MCP authentication: %v", recovered)
				ac.setAuthTerminal(ctx, name, owner, workerErr)
			}
			ac.detachAuth(name, owner, nil).Close()
			ac.completeAuthFlow(name, flow, workerErr)
		}()
		workerErr = ac.runAuth(ctx, cfg, name, m, owner)
	}()
	finish = func(wait context.Context) error {
		select {
		case <-flow.done:
			return flow.err
		case <-wait.Done():
			ac.abortAuthFlow(name, flow, wait.Err())
			return wait.Err()
		}
	}
	cancel = func() { ac.abortAuthFlow(name, flow, context.Canceled) }
	return finish, cancel, nil
}

// runAuthFlow executes the OAuth connect for BeginAuth with browser
// suppression enabled on the freshly created handler.
func (ac *authCoordinator) runAuthFlow(ctx context.Context, cfg ConfigProvider, name string, m config.MCPConfig, owner attemptID) error {
	ac.reg.updateStateFor(name, owner, StateStarting, nil, withPending(m))
	err := ac.reg.connectAndRegister(ctx, cfg, name, m, owner, cfg.Resolver(), channelEnabled(cfg.Overrides().EnabledChannels, name))
	ac.setAuthTerminal(ctx, name, owner, err)
	return err
}

// suppressLock returns the per-server mutex used to serialize
// browser-suppressed OAuth flows, creating it on first use.
func (ac *authCoordinator) suppressLock(name string) *sync.Mutex {
	return &ac.reg.serverLock(name).suppress
}

func (ac *authCoordinator) cancelAuthFlow(name string) {
	ac.authMu.Lock()
	flow := ac.authFlows[name]
	ac.authMu.Unlock()
	if flow != nil {
		ac.abortAuthFlow(name, flow, context.Canceled)
	}
}

// abortAuthFlow bounds the caller's wait and invalidates publication
// immediately. The worker retains the per-server execution slot until it
// actually exits.
func (ac *authCoordinator) abortAuthFlow(name string, flow *authFlow, err error) {
	flow.finishOnce.Do(func() {
		flow.cancel()
		ac.reg.publishMu.Lock()
		if ac.reg.ownsLocked(name, flow.owner) {
			if state, ok := ac.reg.states.Get(name); ok && state.State == StateStarting {
				ac.reg.updateStateLocked(name, StateNeedsAuth, nil, nil, Counts{})
			}
			delete(ac.reg.owners, name)
		}
		auth := ac.detachAuthLocked(name, flow.owner, nil)
		ac.reg.publishMu.Unlock()
		flow.err = err
		close(flow.done)
		auth.Close()
	})
}

// completeAuthFlow is worker-owned: only actual worker exit releases the
// execution slot and removes the exact flow from authFlows.
func (ac *authCoordinator) completeAuthFlow(name string, flow *authFlow, err error) {
	flow.finishOnce.Do(func() {
		flow.cancel()
		flow.err = err
		close(flow.done)
	})
	ac.authMu.Lock()
	if ac.authFlows[name] == flow {
		delete(ac.authFlows, name)
	}
	ac.authMu.Unlock()
	flow.lock.Unlock()
	close(flow.workerDone)
}

func (ac *authCoordinator) detachCurrentAuth(name string) *ownedAuthHandler {
	ac.reg.publishMu.Lock()
	publication, ok := ac.reg.authURLs.Get(name)
	if ok {
		ac.reg.authURLs.Del(name)
	}
	ac.reg.publishMu.Unlock()
	if !ok {
		return nil
	}
	return publication.auth
}

// detachAuth atomically removes only the publication owned by the exact
// generation, attempt, and optional expected handler. The returned ownership
// must be closed outside publishMu unless it is transferred to a session.
func (ac *authCoordinator) detachAuth(name string, owner attemptID, expected *mcpoauth.Handler) *ownedAuthHandler {
	ac.reg.publishMu.Lock()
	auth := ac.detachAuthLocked(name, owner, expected)
	ac.reg.publishMu.Unlock()
	return auth
}

func (ac *authCoordinator) detachAuthLocked(name string, owner attemptID, expected *mcpoauth.Handler) *ownedAuthHandler {
	publication, ok := ac.reg.authURLs.Get(name)
	if !ok || publication.gen != owner.gen || publication.attempt != owner.seq || (expected != nil && publication.auth.handler != expected) {
		return nil
	}
	ac.reg.authURLs.Del(name)
	return publication.auth
}

// clearOAuthToken removes the persisted OAuth token for a named MCP
// server from the global config so subsequent startups don't retry
// with a known-bad refresh token.
func (ac *authCoordinator) clearOAuthToken(cfg ConfigProvider, name string, owner attemptID, expected *oauth.Token) {
	ac.reg.publishMu.Lock()
	if !ac.reg.ownsLocked(name, owner) {
		ac.reg.publishMu.Unlock()
		return
	}
	reservation, ok := ac.reg.tokenReservations[tokenWriteOwner{name: name, attempt: owner}]
	ac.reg.publishMu.Unlock()
	if !ok {
		return
	}
	if _, err := cfg.ClearMCPToken(reservation, expected); err != nil {
		slog.Warn("Failed to clear stale MCP OAuth token", "name", name, "error", err)
	}
}

// AuthenticateMCP initiates the OAuth flow for an MCP server that is in
// StateNeedsAuth. It creates the OAuth handler (which starts a local
// callback server), connects to the server (which triggers the browser
// auth flow on 401), and transitions to StateConnected on success.
func (ac *authCoordinator) AuthenticateMCP(ctx context.Context, cfg ConfigProvider, name string) error {
	m, exists := cfg.Config().MCP[name]
	if !exists {
		return fmt.Errorf("mcp '%s' not found in configuration", name)
	}
	if !usesOAuth(m) {
		return fmt.Errorf("mcp '%s' does not use OAuth authentication", name)
	}

	lock := ac.suppressLock(name)
	if !lock.TryLock() {
		return fmt.Errorf("mcp '%s' already has an authentication in progress", name)
	}
	defer lock.Unlock()
	owner, err := ac.reg.beginAttempt(name)
	if err != nil {
		return err
	}
	defer ac.detachAuth(name, owner, nil).Close()
	ac.reg.updateStateFor(name, owner, StateStarting, nil, withPending(m))
	// This is user initiated; unlike startup it may open a browser.
	ctx = mcpoauth.WithInteractive(ctx)
	err = ac.reg.connectAndRegister(ctx, cfg, name, m, owner, cfg.Resolver(), channelEnabled(cfg.Overrides().EnabledChannels, name))
	ac.setAuthTerminal(ctx, name, owner, err)
	return err
}

// oauthSetup creates and publishes the OAuth handler a session's transport
// uses, gated on the attempt still owning the server: a stale attempt (one
// whose generation or owner no longer matches) gets its handler closed
// immediately instead of published, so a superseded connect can't leave a
// dangling local callback server behind.
func (ac *authCoordinator) oauthSetup(ctx context.Context, cfg ConfigProvider, name string, m config.MCPConfig, gen, attempt uint64, resolver config.VariableResolver, url string) (*mcpoauth.Handler, error) {
	clientID, err := resolver.ResolveValue(m.OAuthClientID)
	if err != nil {
		return nil, fmt.Errorf("oauth_client_id: %w", err)
	}
	clientSecret, err := resolver.ResolveValue(m.OAuthClientSecret)
	if err != nil {
		return nil, fmt.Errorf("oauth_client_secret: %w", err)
	}
	var preregistered *oauth.OAuthClient
	if strings.TrimSpace(clientID) != "" {
		preregistered = &oauth.OAuthClient{ClientID: strings.TrimSpace(clientID), ClientSecret: strings.TrimSpace(clientSecret)}
	}
	owner := attemptID{gen: gen, seq: attempt}
	h, err := mcpoauth.NewHandler(name, strings.TrimRight(url, "/"), m.OAuthToken, preregistered, func(tok *oauth.Token) {
		ac.reg.persistOAuthToken(ctx, cfg, name, owner, tok)
	}, mcpoauth.IsInteractive(ctx), m.OAuthCallbackPort)
	if err != nil {
		return nil, fmt.Errorf("failed to create OAuth handler for mcp %q: %w", name, err)
	}
	ac.reg.publishMu.Lock()
	if ac.reg.currentGen(name) != gen {
		ac.reg.publishMu.Unlock()
		h.Close()
		return nil, errLostOwnership
	}
	if ac.reg.closing || ac.reg.owners[name] != (attemptID{gen: gen, seq: attempt}) {
		ac.reg.publishMu.Unlock()
		h.Close()
		return nil, errLostOwnership
	}
	owned := newOwnedAuthHandler(h)
	old, hadOld := ac.reg.authURLs.Get(name)
	ac.reg.authURLs.Set(name, authPublication{auth: owned, gen: gen, attempt: attempt})
	ac.reg.publishMu.Unlock()
	if hadOld && old.auth != owned {
		old.auth.Close()
	}
	return h, nil
}
