package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/oauth"
	mcpoauth "github.com/rave-soft/braid/internal/oauth/mcp"
	"golang.org/x/oauth2"
)

// suppressBrowserKey marks a context as requesting the OAuth handler not
// open a local browser; the caller surfaces the authorization URL itself.
type suppressBrowserKey struct{}

// PendingAuthServer describes an MCP server awaiting OAuth.
type PendingAuthServer struct {
	Name string
	URL  string
}

// MCPAuthURL returns the current OAuth authorization URL for the named
// MCP, or empty if none is in progress.
func (r *Registry) MCPAuthURL(name string) string {
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	publication, ok := r.authURLs.Get(name)
	if !ok || publication.auth == nil || publication.auth.handler == nil || publication.gen != r.currentGen(name) {
		return ""
	}
	return publication.auth.handler.AuthURL()
}

// PendingAuthMCPs returns MCP servers in StateNeedsAuth with their URLs.
func (r *Registry) PendingAuthMCPs(cfg ConfigProvider) []PendingAuthServer {
	var pending []PendingAuthServer
	for name, info := range r.states.Seq2() {
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
func BeginAuth(cfg ConfigProvider, name string) (finish func(ctx context.Context) error, cancel context.CancelFunc, err error) {
	return defaultRegistry.BeginAuth(cfg, name)
}

type ownedAuthHandler struct {
	handler   *mcpoauth.Handler
	closeOnce sync.Once
	closeFn   func()
}

func newOwnedAuthHandler(handler *mcpoauth.Handler) *ownedAuthHandler {
	return &ownedAuthHandler{handler: handler, closeFn: handler.Close}
}

func (h *ownedAuthHandler) Close() {
	if h != nil {
		h.closeOnce.Do(h.closeFn)
	}
}

type authPublication struct {
	auth    *ownedAuthHandler
	gen     uint64
	attempt uint64
}

type authFlow struct {
	cancel     context.CancelFunc
	done       chan struct{}
	workerDone chan struct{}
	owner      attemptID
	lock       *sync.Mutex
	err        error
	finishOnce sync.Once
}

func (r *Registry) BeginAuth(cfg ConfigProvider, name string) (finish func(ctx context.Context) error, cancel context.CancelFunc, err error) {
	m, exists := cfg.Config().MCP[name]
	if !exists {
		return nil, nil, fmt.Errorf("mcp '%s' not found in configuration", name)
	}
	if !usesOAuth(m) {
		return nil, nil, fmt.Errorf("mcp '%s' does not use OAuth authentication", name)
	}
	lock := r.suppressLock(name)
	if !lock.TryLock() {
		return nil, nil, fmt.Errorf("mcp '%s' already has an authentication in progress", name)
	}
	ctx, flowCancel := context.WithCancel(context.Background())
	ctx = mcpoauth.WithInteractive(context.WithValue(ctx, suppressBrowserKey{}, true))
	owner, ownerErr := r.beginAttempt(name)
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
	r.authMu.Lock()
	r.authFlows[name] = flow
	r.authMu.Unlock()
	go func() {
		var workerErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				workerErr = fmt.Errorf("panic in MCP authentication: %v", recovered)
				r.setAuthTerminal(name, owner, workerErr)
			}
			r.detachAuth(name, owner, nil).Close()
			r.completeAuthFlow(name, flow, workerErr)
		}()
		workerErr = r.runAuth(ctx, cfg, name, m, owner)
	}()
	finish = func(wait context.Context) error {
		select {
		case <-flow.done:
			return flow.err
		case <-wait.Done():
			r.abortAuthFlow(name, flow, wait.Err())
			return wait.Err()
		}
	}
	cancel = func() { r.abortAuthFlow(name, flow, context.Canceled) }
	return finish, cancel, nil
}

// runAuthFlow executes the OAuth connect for BeginAuth with browser
// suppression enabled on the freshly created handler.
func (r *Registry) runAuthFlow(ctx context.Context, cfg ConfigProvider, name string, m config.MCPConfig, owner attemptID) error {
	r.updateStateFor(name, owner, StateStarting, nil, withPending(m))
	err := r.connectAndRegister(ctx, cfg, name, m, owner, cfg.Resolver(), channelEnabled(cfg.Overrides().EnabledChannels, name))
	r.setAuthTerminal(name, owner, err)
	return err
}

// suppressLock returns the per-server mutex used to serialize
// browser-suppressed OAuth flows, creating it on first use.
func (r *Registry) suppressLock(name string) *sync.Mutex {
	return r.suppressMus.GetOrSet(name, func() *sync.Mutex { return &sync.Mutex{} })
}

func (r *Registry) cancelAuthFlow(name string) {
	r.authMu.Lock()
	flow := r.authFlows[name]
	r.authMu.Unlock()
	if flow != nil {
		r.abortAuthFlow(name, flow, context.Canceled)
	}
}

// abortAuthFlow bounds the caller's wait and invalidates publication
// immediately. The worker retains the per-server execution slot until it
// actually exits.
func (r *Registry) abortAuthFlow(name string, flow *authFlow, err error) {
	flow.finishOnce.Do(func() {
		flow.cancel()
		r.publishMu.Lock()
		if r.ownsLocked(name, flow.owner) {
			if state, ok := r.states.Get(name); ok && state.State == StateStarting {
				r.updateStateLocked(name, StateNeedsAuth, nil, nil, Counts{})
			}
			delete(r.owners, name)
		}
		auth := r.detachAuthLocked(name, flow.owner, nil)
		r.publishMu.Unlock()
		flow.err = err
		close(flow.done)
		auth.Close()
	})
}

// completeAuthFlow is worker-owned: only actual worker exit releases the
// execution slot and removes the exact flow from authFlows.
func (r *Registry) completeAuthFlow(name string, flow *authFlow, err error) {
	flow.finishOnce.Do(func() {
		flow.cancel()
		flow.err = err
		close(flow.done)
	})
	r.authMu.Lock()
	if r.authFlows[name] == flow {
		delete(r.authFlows, name)
	}
	r.authMu.Unlock()
	flow.lock.Unlock()
	close(flow.workerDone)
}

func (r *Registry) detachCurrentAuth(name string) *ownedAuthHandler {
	r.publishMu.Lock()
	publication, ok := r.authURLs.Get(name)
	if ok {
		r.authURLs.Del(name)
	}
	r.publishMu.Unlock()
	if !ok {
		return nil
	}
	return publication.auth
}

// detachAuth atomically removes only the publication owned by the exact
// generation, attempt, and optional expected handler. The returned ownership
// must be closed outside publishMu unless it is transferred to a session.
func (r *Registry) detachAuth(name string, owner attemptID, expected *mcpoauth.Handler) *ownedAuthHandler {
	r.publishMu.Lock()
	auth := r.detachAuthLocked(name, owner, expected)
	r.publishMu.Unlock()
	return auth
}

func (r *Registry) detachAuthLocked(name string, owner attemptID, expected *mcpoauth.Handler) *ownedAuthHandler {
	publication, ok := r.authURLs.Get(name)
	if !ok || publication.gen != owner.gen || publication.attempt != owner.seq || (expected != nil && publication.auth.handler != expected) {
		return nil
	}
	r.authURLs.Del(name)
	return publication.auth
}

// usesOAuth is deliberately transport-agnostic: both HTTP transports share the
// same startup, renewal and explicit-auth policy.
func usesOAuth(m config.MCPConfig) bool {
	return m.OAuth && (m.Type == config.MCPHttp || m.Type == config.MCPSSE)
}

// hasUsableToken returns true if the saved OAuth token has an access
// token that can be used or refreshed. A token with an empty access
// token is structurally invalid and should be treated as missing.
func hasUsableToken(tok *oauth.Token) bool {
	return tok != nil && tok.AccessToken != ""
}

// isOAuthInitErr returns true if the error indicates the OAuth token
// is missing, no longer valid, or cannot be refreshed. This covers:
//   - invalid_grant: expired or revoked refresh tokens
//   - invalid_client: deleted or deactivated client registrations
//   - "no token available": the handler had no cached token to use
//   - interactive authorization was required but withheld during startup
func isOAuthInitErr(err error) bool {
	if errors.Is(err, mcpoauth.ErrInteractiveAuthRequired) {
		return true
	}
	var rErr *oauth2.RetrieveError
	if errors.As(err, &rErr) {
		return rErr.ErrorCode == "invalid_grant" || rErr.ErrorCode == "invalid_client"
	}
	msg := err.Error()
	return strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "invalid_client") ||
		strings.Contains(msg, "no token available")
}

// clearOAuthToken removes the persisted OAuth token for a named MCP
// server from the global config so subsequent startups don't retry
// with a known-bad refresh token.
func (r *Registry) clearOAuthToken(cfg ConfigProvider, name string, owner attemptID, expected *oauth.Token) {
	r.publishMu.Lock()
	if !r.ownsLocked(name, owner) {
		r.publishMu.Unlock()
		return
	}
	reservation, ok := r.tokenReservations[tokenWriteOwner{name: name, attempt: owner}]
	r.publishMu.Unlock()
	if !ok {
		return
	}
	if _, err := cfg.ClearMCPToken(reservation, expected); err != nil {
		slog.Warn("Failed to clear stale MCP OAuth token", "name", name, "error", err)
	}
}
