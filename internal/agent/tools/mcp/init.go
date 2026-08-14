// Package mcp provides functionality for managing Model Context Protocol (MCP)
// clients within the Braid application.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/home"
	"github.com/rave-soft/braid/internal/oauth"
	mcpoauth "github.com/rave-soft/braid/internal/oauth/mcp"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/version"
	"golang.org/x/oauth2"
)

// parseLevel converts an MCP logging level string to a slog.Level. The
// entire MCP logging feature is deprecated per SEP-2577 but remains
// functional; servers may still send log notifications during the
// deprecation window.
func parseLevel(level string) slog.Level {
	switch level {
	case "info":
		return slog.LevelInfo
	case "notice":
		return slog.LevelInfo
	case "warning":
		return slog.LevelWarn
	default:
		return slog.LevelDebug
	}
}

// ClientSession wraps an mcp.ClientSession with a context cancel function so
// that the context created during session establishment is properly cleaned up
// on close.
type ClientSession struct {
	*mcp.ClientSession
	cancel    context.CancelFunc
	auth      *ownedAuthHandler
	closeIdle func()
}

// Close cancels the session context and then closes the underlying session.
func (s *ClientSession) Close() error {
	s.cancel()
	if s.auth != nil {
		s.auth.Close()
	}
	if s.closeIdle != nil {
		s.closeIdle()
	}
	return s.ClientSession.Close()
}

// suppressBrowserKey marks a context as requesting the OAuth handler not
// open a local browser; the caller surfaces the authorization URL itself.
type suppressBrowserKey struct{}

// renewLock returns the per-server mutex used to serialize session renewals,
// creating it on first use.
func (r *Registry) renewLock(name string) *sync.Mutex {
	r.renewMusMu.Lock()
	defer r.renewMusMu.Unlock()
	mu, ok := r.renewMus[name]
	if !ok {
		mu = &sync.Mutex{}
		r.renewMus[name] = mu
	}
	return mu
}

// State represents the current state of an MCP client
type State int

const (
	StateDisabled State = iota
	StateStarting
	StateConnected
	StateError
	StateNeedsAuth
)

func (s State) String() string {
	switch s {
	case StateDisabled:
		return "disabled"
	case StateStarting:
		return "starting"
	case StateConnected:
		return "connected"
	case StateError:
		return "error"
	case StateNeedsAuth:
		return "needs auth"
	default:
		return "unknown"
	}
}

// EventType represents the type of MCP event
type EventType uint

const (
	EventStateChanged EventType = iota
	EventToolsListChanged
	EventPromptsListChanged
	EventResourcesListChanged
	// EventChannelMessage is published when a channel server pushes a
	// notifications/claude/channel event. ChannelMessage carries the rendered,
	// escaped <channel> element ready for injection into the session.
	EventChannelMessage
)

// Event represents an event in the MCP system
type Event struct {
	Type   EventType
	Name   string
	State  State
	Error  error
	Counts Counts
	// ChannelMessage is set only for EventChannelMessage: the fully rendered
	// and escaped <channel>...</channel> element to inject into the session.
	ChannelMessage string
}

// Counts number of available tools, prompts, etc.
type Counts struct {
	Tools     int
	Prompts   int
	Resources int
}

// ClientInfo holds information about an MCP client's state.
type ClientInfo struct {
	Name        string
	State       State
	Error       error
	Client      *ClientSession
	Counts      Counts
	ConnectedAt time.Time

	// Config is the configuration the server last successfully connected
	// with. Reconcile compares it against the live config to decide whether
	// a connected server needs a restart. It is recorded by updateState on
	// StateConnected and cleared on StateDisabled, so it never has to be
	// synced by hand.
	Config config.MCPConfig

	// PendingConfig is the configuration an in-flight initialization is
	// connecting with. It is set on StateStarting so a config change that
	// arrives mid-connect is compared against the attempt actually in
	// progress rather than the last successful one, which would leave the
	// server skipped as "starting" and never restarted for the new config.
	PendingConfig *config.MCPConfig
}

// InitializeSingle initializes a single MCP client by name.
func InitializeSingle(ctx context.Context, name string, cfg *config.ConfigStore) error {
	return defaultRegistry.InitializeSingle(ctx, name, cfg)
}

func (r *Registry) InitializeSingle(ctx context.Context, name string, cfg *config.ConfigStore) error {
	m, exists := cfg.Config().MCP[name]
	if !exists {
		return fmt.Errorf("mcp '%s' not found in configuration", name)
	}

	if m.Disabled {
		r.updateState(name, StateDisabled, nil, nil, Counts{})
		slog.Debug("Skipping disabled MCP", "name", name)
		return nil
	}

	owner, err := r.beginAttempt(name)
	if err != nil {
		return err
	}
	return r.initClient(ctx, cfg, name, m, owner, cfg.Resolver())
}

// AuthenticateMCP initiates the OAuth flow for an MCP server that is in
// StateNeedsAuth. It creates the OAuth handler (which starts a local
// callback server), connects to the server (which triggers the browser
// auth flow on 401), and transitions to StateConnected on success.
func AuthenticateMCP(ctx context.Context, cfg *config.ConfigStore, name string) error {
	return defaultRegistry.AuthenticateMCP(ctx, cfg, name)
}

func (r *Registry) AuthenticateMCP(ctx context.Context, cfg *config.ConfigStore, name string) error {
	m, exists := cfg.Config().MCP[name]
	if !exists {
		return fmt.Errorf("mcp '%s' not found in configuration", name)
	}
	if !usesOAuth(m) {
		return fmt.Errorf("mcp '%s' does not use OAuth authentication", name)
	}

	lock := r.suppressLock(name)
	if !lock.TryLock() {
		return fmt.Errorf("mcp '%s' already has an authentication in progress", name)
	}
	defer lock.Unlock()
	owner, err := r.beginAttempt(name)
	if err != nil {
		return err
	}
	defer r.detachAuth(name, owner, nil).Close()
	r.updateStateFor(name, owner, StateStarting, nil, withPending(m))
	// This is user initiated; unlike startup it may open a browser.
	ctx = mcpoauth.WithInteractive(ctx)
	err = r.connectAndRegister(ctx, cfg, name, m, owner, cfg.Resolver(), channelEnabled(cfg.Overrides().EnabledChannels, name))
	r.setAuthTerminal(name, owner, err)
	return err
}

// PendingAuthServer describes an MCP server awaiting OAuth.
type PendingAuthServer struct {
	Name string
	URL  string
}

// MCPAuthURL returns the current OAuth authorization URL for the named
// MCP, or empty if none is in progress.
func MCPAuthURL(name string) string { return defaultRegistry.MCPAuthURL(name) }

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
func PendingAuthMCPs(cfg *config.ConfigStore) []PendingAuthServer {
	return defaultRegistry.PendingAuthMCPs(cfg)
}

func (r *Registry) PendingAuthMCPs(cfg *config.ConfigStore) []PendingAuthServer {
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
func BeginAuth(cfg *config.ConfigStore, name string) (finish func(ctx context.Context) error, cancel context.CancelFunc, err error) {
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

func (r *Registry) BeginAuth(cfg *config.ConfigStore, name string) (finish func(ctx context.Context) error, cancel context.CancelFunc, err error) {
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
func (r *Registry) runAuthFlow(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, owner attemptID) error {
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

// initClient initializes a single MCP client with the given configuration.
// gen is the server generation captured when the attempt was launched; the
// resulting session is only committed if the generation is still current, so
// a config change that restarts the server mid-connect discards this attempt.
func (r *Registry) initClient(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, owner attemptID, resolver config.VariableResolver) error {
	defer r.detachAuth(name, owner, nil).Close()
	if usesOAuth(m) && !r.reserveTokenMutation(cfg, name, m, owner) {
		return context.Canceled
	}
	// OAuth MCPs without a usable cached token require user interaction
	// (browser auth). If a cached token exists with an access token
	// (even if expired), try connecting first so the SDK can attempt a
	// silent refresh. Only defer to the UI if no token is available at
	// all or the token is structurally invalid (empty access token).
	if usesOAuth(m) && !hasUsableToken(m.OAuthToken) {
		if m.OAuthToken != nil {
			r.clearOAuthToken(cfg, name, owner, m.OAuthToken)
		}
		r.updateStateFor(name, owner, StateNeedsAuth, nil)
		r.clearMCPDataFor(name, owner)
		slog.Info("MCP server requires OAuth authentication", "name", name)
		return nil
	}

	r.updateStateFor(name, owner, StateStarting, nil, withPending(m))
	err := r.connectAndRegister(ctx, cfg, name, m, owner, resolver, channelEnabled(cfg.Overrides().EnabledChannels, name))
	if err != nil {
		// If an OAuth MCP fails because the saved token is no longer
		// valid (e.g. refresh token expired or revoked) or no token
		// could be obtained, clear the stale token and prompt the user
		// to re-authenticate instead of leaving the server stuck in
		// StateError.
		if usesOAuth(m) && isOAuthInitErr(err) {
			if m.OAuthToken != nil {
				r.clearOAuthToken(cfg, name, owner, m.OAuthToken)
			}
			r.updateStateFor(name, owner, StateNeedsAuth, nil)
			slog.Info("MCP OAuth token is no longer valid, re-authentication required", "name", name, "error", err)
			return nil
		}
		// Setup/listing errors must settle the current attempt; otherwise the UI
		// remains permanently in StateStarting.
		r.updateStateFor(name, owner, StateError, maybeTimeoutErr(err, mcpTimeout(m)))
		return err
	}
	return nil
}

// connectAndRegister creates a session, lists tools and prompts,
// registers them in global state, and transitions to StateConnected.
//
// gen is the generation captured when this attempt was launched. If the
// server was torn down since (generation bumped), the freshly built session
// is closed and discarded instead of being registered over whatever the
// newer attempt is doing. This is what makes a config change that lands
// mid-connect converge on the latest config rather than a stale one.
func (r *Registry) connectAndRegister(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, owner attemptID, resolver config.VariableResolver, channelOptIn bool) error {
	if usesOAuth(m) && !r.reserveTokenMutation(cfg, name, m, owner) {
		return context.Canceled
	}
	session, err := r.createSession(ctx, cfg, name, m, owner, resolver, channelOptIn)
	if err != nil {
		return err
	}
	return r.publishOrClose(ctx, name, m, owner, session)
}

func (r *Registry) publishOrClose(ctx context.Context, name string, m config.MCPConfig, owner attemptID, session *ClientSession) error {
	committed := false
	defer func() {
		if !committed {
			r.closeSession(name, session)
		}
	}()

	// A teardown ran while we were connecting: a newer attempt owns this
	// server now. Bail before writing to any shared registry so we don't
	// clobber the newer attempt's registrations; just drop our own session.
	if !r.owns(name, owner) {
		slog.Debug("Discarding stale MCP session after config change", "name", name)
		return context.Canceled
	}

	if err := r.publishSession(ctx, name, m, owner, session); err != nil {
		return err
	}
	committed = true
	return nil
}

func (r *Registry) publishSession(ctx context.Context, name string, m config.MCPConfig, owner attemptID, session *ClientSession) error {
	tools, err := getTools(ctx, session)
	if err != nil {
		return err
	}
	prompts, err := getPrompts(ctx, session)
	if err != nil {
		return err
	}
	resources, err := getResources(ctx, session)
	if err != nil {
		return err
	}
	tools = filterTools(m, tools)
	if !r.owns(name, owner) {
		return context.Canceled
	}
	r.publishMu.Lock()
	if !r.ownsLocked(name, owner) {
		r.publishMu.Unlock()
		return context.Canceled
	}
	if session.auth != nil && r.detachAuthLocked(name, owner, session.auth.handler) != session.auth {
		r.publishMu.Unlock()
		return context.Canceled
	}
	r.catalogMu.Lock()
	if len(tools) == 0 {
		r.allTools.Del(name)
	} else {
		r.allTools.Set(name, tools)
	}
	if len(prompts) == 0 {
		r.allPrompts.Del(name)
	} else {
		r.allPrompts.Set(name, prompts)
	}
	if len(resources) == 0 {
		r.allResources.Del(name)
	} else {
		r.allResources.Set(name, resources)
	}
	r.catalogChanged()
	r.catalogMu.Unlock()
	old, hadOld := r.sessions.Get(name)
	r.sessions.Set(name, session)
	r.sessionOwners[name] = owner
	r.updateStateLocked(name, StateConnected, nil, session, Counts{Tools: len(tools), Prompts: len(prompts), Resources: len(resources)}, withConfig(m))
	r.publishMu.Unlock()
	if hadOld && old != session {
		r.closeSession(name, old)
	}
	return nil
}

// persistOAuthToken saves the OAuth token from a session to the global
// config so it survives restarts.

// DisableSingle disables and closes a single MCP client by name.
func DisableSingle(cfg *config.ConfigStore, name string) error {
	return defaultRegistry.DisableSingle(cfg, name)
}

func (r *Registry) DisableSingle(cfg *config.ConfigStore, name string) error {
	// teardown bumps the generation, invalidating any in-flight connect, and
	// the StateDisabled transition clears the recorded config so a later
	// re-enable (even with an unchanged config) is seen as new and restarts.
	r.teardown(name)
	r.updateState(name, StateDisabled, nil, nil, Counts{})
	slog.Info("Disabled mcp client", "name", name)
	return nil
}

// goInitClient launches initClient in a goroutine with panic recovery.
// Shared by Initialize and Reinitialize so the panic-to-state policy
// lives in one place. wg, if non-nil, is Done when the attempt finishes
// (success or failure); Initialize uses it to await startup. The goroutine
// captures the server's generation at launch so a concurrent teardown
// invalidates its result rather than letting it register a stale session.
func (r *Registry) goInitClient(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, wg *sync.WaitGroup) {
	owner, err := r.beginAttempt(name)
	if err != nil {
		if wg != nil {
			wg.Done()
		}
		return
	}
	go func() {
		if wg != nil {
			defer wg.Done()
		}
		defer func() {
			r.detachAuth(name, owner, nil).Close()
			if rec := recover(); rec != nil {
				var err error
				switch v := rec.(type) {
				case error:
					err = v
				case string:
					err = fmt.Errorf("panic: %s", v)
				default:
					err = fmt.Errorf("panic: %v", v)
				}
				r.updateStateFor(name, owner, StateError, err)
				slog.Error("Panic in MCP client initialization", "error", err, "name", name)
			}
		}()
		if err := r.initClient(ctx, cfg, name, m, owner, cfg.Resolver()); err != nil {
			slog.Debug("Failed to initialize MCP client", "name", name, "error", err)
		}
	}()
}

// currentGen returns a server's current generation without bumping it.
func (r *Registry) currentGen(name string) uint64 {
	g, _ := r.gens.Get(name)
	return g
}

func (r *Registry) beginAttempt(name string) (attemptID, error) {
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	if r.closing {
		return attemptID{}, errors.New("mcp registry is closing")
	}
	owner := attemptID{gen: r.currentGen(name), seq: r.authAttempt.Add(1)}
	r.owners[name] = owner
	return owner, nil
}

func (r *Registry) reserveTokenMutation(cfg *config.ConfigStore, name string, m config.MCPConfig, owner attemptID) bool {
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	if !r.ownsLocked(name, owner) {
		return false
	}
	key := tokenWriteOwner{name: name, attempt: owner}
	if _, ok := r.tokenReservations[key]; ok {
		return true
	}
	reservation, ok := cfg.ReserveMCPTokenMutation(name, m)
	if ok {
		r.tokenReservations[key] = &reservation
	}
	return ok
}

func (r *Registry) owns(name string, owner attemptID) bool {
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	return r.ownsLocked(name, owner)
}

func (r *Registry) ownsLocked(name string, owner attemptID) bool {
	return !r.closing && owner.valid() && r.currentGen(name) == owner.gen && r.owners[name] == owner
}

func (r *Registry) beginTokenWrite(name string, owner attemptID) *tokenWrite {
	if r.beforeTokenPersist != nil {
		r.beforeTokenPersist()
	}
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	state, hasState := r.states.Get(name)
	key := tokenWriteOwner{name: name, attempt: owner}
	if !r.ownsLocked(name, owner) || (hasState && state.State == StateDisabled) {
		return nil
	}
	if _, ok := r.tokenReservations[key]; !ok {
		return nil
	}
	write := &tokenWrite{done: make(chan struct{})}
	if r.tokenWrites[key] == nil {
		r.tokenWrites[key] = map[*tokenWrite]struct{}{}
	}
	r.tokenWrites[key][write] = struct{}{}
	return write
}

func (r *Registry) finishTokenWrite(name string, owner attemptID, write *tokenWrite) {
	r.publishMu.Lock()
	key := tokenWriteOwner{name: name, attempt: owner}
	delete(r.tokenWrites[key], write)
	if len(r.tokenWrites[key]) == 0 {
		delete(r.tokenWrites, key)
	}
	close(write.done)
	r.publishMu.Unlock()
}

func (r *Registry) persistOAuthToken(ctx context.Context, cfg *config.ConfigStore, name string, owner attemptID, tok *oauth.Token) {
	if m, ok := cfg.Config().MCP[name]; ok && !r.reserveTokenMutation(cfg, name, m, owner) {
		return
	}
	write := r.beginTokenWrite(name, owner)
	if write == nil {
		return
	}
	defer r.finishTokenWrite(name, owner, write)
	key := fmt.Sprintf("mcp.%s.oauth_token", name)
	if err := r.tokenPersist(ctx, cfg, key, tok); err != nil {
		slog.Warn("Failed to persist MCP OAuth token", "name", name, "error", err)
		return
	}
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	if !r.ownsLocked(name, owner) {
		return
	}
	reservation, ok := r.tokenReservations[tokenWriteOwner{name: name, attempt: owner}]
	if !ok {
		return
	}
	if err := r.tokenCommit(cfg, reservation, tok); err != nil {
		slog.Warn("Failed to persist MCP OAuth token", "name", name, "error", err)
	}
}

func (r *Registry) tokenWriteWaitersLocked(name string) []<-chan struct{} {
	var waiters []<-chan struct{}
	for owner, writes := range r.tokenWrites {
		if name != "" && owner.name != name {
			continue
		}
		for write := range writes {
			waiters = append(waiters, write.done)
		}
	}
	return waiters
}

func waitTokenWrites(ctx context.Context, waiters []<-chan struct{}) error {
	for _, done := range waiters {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (r *Registry) sessionOwner(name string) (attemptID, *ClientSession, bool) {
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	session, ok := r.sessions.Get(name)
	return r.sessionOwners[name], session, ok && !r.closing
}

func (r *Registry) ownsSessionLocked(name string, owner attemptID, session *ClientSession) bool {
	current, ok := r.sessions.Get(name)
	return !r.closing && owner.valid() && r.currentGen(name) == owner.gen && ok && r.sessionOwners[name] == owner && current == session
}

func (r *Registry) beginRenewal(name string, owner attemptID, session *ClientSession, pingErr error) (attemptID, bool) {
	r.publishMu.Lock()
	if !r.ownsSessionLocked(name, owner, session) {
		r.publishMu.Unlock()
		return attemptID{}, false
	}
	renewal := attemptID{gen: owner.gen, seq: r.authAttempt.Add(1)}
	state, _ := r.states.Get(name)
	cleanup := r.updateStateLocked(name, StateError, pingErr, nil, state.Counts)
	r.owners[name] = renewal
	r.publishMu.Unlock()
	r.runStateCleanup(name, cleanup)
	return renewal, true
}

func (r *Registry) updateStateForSession(name string, owner attemptID, session *ClientSession, state State, err error, counts Counts) {
	r.publishMu.Lock()
	if !r.ownsSessionLocked(name, owner, session) {
		r.publishMu.Unlock()
		return
	}
	cleanup := r.updateStateLocked(name, state, err, nil, counts)
	r.publishMu.Unlock()
	r.runStateCleanup(name, cleanup)
}

func (r *Registry) setAuthTerminal(name string, owner attemptID, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, context.Canceled) || isOAuthInitErr(err) {
		r.updateStateFor(name, owner, StateNeedsAuth, nil)
		return
	}
	r.updateStateFor(name, owner, StateError, err)
}

// teardown closes a server's session and clears its tools, prompts,
// resources, and auth state, then bumps the server's generation so any
// in-flight initialization for it is discarded on commit. It leaves the
// states entry intact; callers decide whether to delete or update it.
// Shared by DisableSingle, removeServer, and the restart path in
// Reinitialize.
func (r *Registry) teardown(name string) {
	// Invalidate and unpublish atomically before waiting for a worker. A stale
	// worker therefore cannot publish after disable/remove, even if it ignores
	// cancellation until its transport operation returns.
	r.publishMu.Lock()
	g := r.currentGen(name) + 1
	r.gens.Set(name, g)
	delete(r.owners, name)
	for key := range r.tokenReservations {
		if key.name == name {
			delete(r.tokenReservations, key)
		}
	}
	session, hasSession := r.sessions.Take(name)
	delete(r.sessionOwners, name)
	r.catalogMu.Lock()
	r.allTools.Del(name)
	r.allPrompts.Del(name)
	r.allResources.Del(name)
	r.catalogChanged()
	r.catalogMu.Unlock()
	waiters := r.tokenWriteWaitersLocked(name)
	r.publishMu.Unlock()
	r.cancelAuthFlow(name)
	r.detachCurrentAuth(name).Close()
	if hasSession {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), lifecycleCleanupTimeout)
		defer cancel()
		r.closeSessionContext(cleanupCtx, name, session)
	}
	_ = waitTokenWrites(context.Background(), waiters)
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

func (r *Registry) clearMCPDataFor(name string, owner attemptID) {
	r.publishMu.Lock()
	if !r.ownsLocked(name, owner) {
		r.publishMu.Unlock()
		return
	}
	r.catalogMu.Lock()
	r.allTools.Del(name)
	r.allPrompts.Del(name)
	r.allResources.Del(name)
	r.catalogChanged()
	r.catalogMu.Unlock()
	r.publishMu.Unlock()
	r.detachAuth(name, owner, nil).Close()
}

func (r *Registry) getOrRenewClient(ctx context.Context, cfg *config.ConfigStore, name string) (*ClientSession, error) {
	m := cfg.Config().MCP[name]
	timeout := mcpTimeout(m)

	observedOwner, observedSession, observed := r.sessionOwner(name)
	var pingErr error
	if observed {
		pingErr = r.ping(ctx, observedSession, timeout)
		if pingErr == nil {
			r.publishMu.Lock()
			current := r.ownsSessionLocked(name, observedOwner, observedSession)
			r.publishMu.Unlock()
			if current {
				return observedSession, nil
			}
		}
	}

	mu := r.renewLock(name)
	mu.Lock()
	defer mu.Unlock()

	owner, session, ok := r.sessionOwner(name)
	if !ok {
		return nil, fmt.Errorf("mcp '%s' not available", name)
	}
	if !observed || owner != observedOwner || session != observedSession {
		if err := r.ping(ctx, session, timeout); err != nil {
			return nil, context.Canceled
		}
		r.publishMu.Lock()
		current := r.ownsSessionLocked(name, owner, session)
		r.publishMu.Unlock()
		if current {
			return session, nil
		}
		return nil, context.Canceled
	}

	renewal, ok := r.beginRenewal(name, observedOwner, observedSession, maybeTimeoutErr(pingErr, timeout))
	if !ok {
		return nil, context.Canceled
	}
	if usesOAuth(m) && !r.reserveTokenMutation(cfg, name, m, renewal) {
		return nil, context.Canceled
	}
	newSess, err := r.newSession(ctx, cfg, name, m, renewal, cfg.Resolver(), channelEnabled(cfg.Overrides().EnabledChannels, name))
	if err != nil {
		r.clearMCPDataFor(name, renewal)
		if usesOAuth(m) && isOAuthInitErr(err) {
			if m.OAuthToken != nil {
				r.clearOAuthToken(cfg, name, renewal, m.OAuthToken)
			}
			r.updateStateFor(name, renewal, StateNeedsAuth, nil)
			slog.Info("MCP OAuth session expired, re-authentication required", "name", name, "error", err)
		} else {
			r.updateStateFor(name, renewal, StateError, maybeTimeoutErr(err, timeout))
		}
		return nil, err
	}
	if err := r.publishOrClose(ctx, name, m, renewal, newSess); err != nil {
		if !errors.Is(err, context.Canceled) {
			r.updateStateFor(name, renewal, StateError, err)
		}
		return nil, err
	}
	return newSess, nil
}

// pingSession pings a session with the server's configured timeout.
func (r *Registry) pingSession(ctx context.Context, s *ClientSession, timeout time.Duration) error {
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.Ping(pingCtx, nil)
}

// closeSession closes an MCP session, logging only unexpected errors. EOF,
// context cancellation, and a killed child are the ordinary result of tearing
// a session down and are not worth surfacing.
func (r *Registry) closeSessionContext(ctx context.Context, name string, s *ClientSession) {
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) && err.Error() != "signal: killed" {
			slog.Warn("Error closing MCP session", "name", name, "error", err)
		}
	case <-ctx.Done():
	}
}

func (r *Registry) closeSession(name string, s *ClientSession) {
	ctx, cancel := context.WithTimeout(context.Background(), lifecycleCleanupTimeout)
	defer cancel()
	r.closeSessionContext(ctx, name, s)
}

// stateOpt mutates the ClientInfo a transition is about to publish. Config
// recording is opt-in: only the sites that actually own a config (the connect
// and starting paths) pass one, so the many error and count-refresh call sites
// can't accidentally clobber the recorded config by passing a zero value.
type stateOpt func(*ClientInfo)

// withConfig records the config now in effect. Used on StateConnected.
func withConfig(m config.MCPConfig) stateOpt {
	return func(i *ClientInfo) {
		i.Config = m
		i.PendingConfig = nil
	}
}

// withPending records the config an in-flight attempt is connecting with.
// Used on StateStarting.
func withPending(m config.MCPConfig) stateOpt {
	return func(i *ClientInfo) {
		mc := m
		i.PendingConfig = &mc
	}
}

// updateState updates the state of an MCP client and publishes an event.
//
// Config bookkeeping is split between the caller and the state machine:
//   - Callers that own a config opt in via withConfig (StateConnected) or
//     withPending (StateStarting). Everyone else leaves the recorded config
//     untouched, so an error or count refresh can't wipe it.
//   - The state machine owns the transitions with fixed semantics:
//     StateDisabled clears both so a later re-enable with an unchanged config
//     is seen as new rather than skipped as already initialized.
//
// usesOAuth is deliberately transport-agnostic: both HTTP transports share the
// same startup, renewal and explicit-auth policy.
func usesOAuth(m config.MCPConfig) bool {
	return m.OAuth && (m.Type == config.MCPHttp || m.Type == config.MCPSSE)
}

// updateStateFor publishes a transition only while its attempt still owns the
// server. Callers that do not belong to an asynchronous attempt use
// updateState; lifecycle paths bump generation before publishing instead.
func (r *Registry) updateStateFor(name string, owner attemptID, state State, err error, opts ...stateOpt) {
	r.publishMu.Lock()
	if !r.ownsLocked(name, owner) {
		r.publishMu.Unlock()
		return
	}
	cleanup := r.updateStateLocked(name, state, err, nil, Counts{}, opts...)
	r.publishMu.Unlock()
	r.runStateCleanup(name, cleanup)
}

func (r *Registry) updateState(name string, state State, err error, client *ClientSession, counts Counts, opts ...stateOpt) {
	r.publishMu.Lock()
	cleanup := r.updateStateLocked(name, state, err, client, counts, opts...)
	r.publishMu.Unlock()
	r.runStateCleanup(name, cleanup)
}

type stateCleanup struct {
	session *ClientSession
}

func (r *Registry) runStateCleanup(name string, cleanup stateCleanup) {
	if cleanup.session != nil {
		r.closeSession(name, cleanup.session)
	}
}

func (r *Registry) updateStateLocked(name string, state State, err error, client *ClientSession, counts Counts, opts ...stateOpt) stateCleanup {
	var cleanup stateCleanup
	prev, _ := r.states.Get(name)
	info := prev
	info.Name = name
	info.State = state
	info.Error = err
	info.Client = client
	info.Counts = counts
	for _, opt := range opts {
		opt(&info)
	}
	switch state {
	case StateConnected:
		info.ConnectedAt = time.Now()
	case StateDisabled:
		info.Config = config.MCPConfig{}
		info.PendingConfig = nil
	case StateError:
		// StateError is terminal for the publishing attempt. Detach its side-
		// effect ownership together with the session and catalogs so late work
		// cannot republish after the transition.
		delete(r.owners, name)
		// A session that has errored is dead to us. Atomically remove it and
		// close it so the child process and its stdio pipes are released — the
		// bare map delete this used to do leaked both. Clearing the tool
		// registry keeps the agent from advertising tools it can no longer
		// call: without it, braid_info / the `/mcp` menu and the tool list
		// handed to the LLM diverge, so a server still reads "connected, N
		// tools" while every call fails with "tool not found".
		if old, ok := r.sessions.Take(name); ok {
			delete(r.sessionOwners, name)
			cleanup.session = old
		}
		// Drop every registry entry for the dead server. Leaving prompts or
		// resources behind lets a disconnected server keep advertising
		// capabilities the agent can no longer fulfil, the same divergence the
		// tool clear prevents.
		r.clearCatalog(name)
	}
	r.states.Set(name, info)

	// Publish state change event
	r.broker.Publish(pubsub.UpdatedEvent, Event{
		Type:   EventStateChanged,
		Name:   name,
		State:  state,
		Error:  err,
		Counts: counts,
	})
	return cleanup
}

func (r *Registry) createSession(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, owner attemptID, resolver config.VariableResolver, channelOptIn bool) (*ClientSession, error) {
	timeout := mcpTimeout(m)
	mcpCtx, cancel := context.WithCancel(ctx)
	cancelTimer := time.AfterFunc(timeout, cancel)

	transport, oauthHandler, err := r.createTransportFor(mcpCtx, cfg, name, m, owner.gen, owner.seq, resolver)
	if err != nil {
		cancel()
		cancelTimer.Stop()
		return nil, err
	}

	// If the caller requested a browser-suppressed flow (server-driven
	// remote auth), suppress the handler's local browser open; the caller
	// surfaces MCPAuthURL(name) to the user on their own machine.
	if oauthHandler != nil {
		if suppress, _ := ctx.Value(suppressBrowserKey{}).(bool); suppress {
			oauthHandler.SetBrowserSuppress(true)
		}
	}

	// Wrap the transport so channel notifications can be intercepted. The
	// gate starts undecided: notifications that arrive during capability
	// negotiation are buffered. After Connect resolves, the gate is opened
	// (and the buffer drained) only when the server declares the channel
	// capability AND was opted in via --channels; otherwise it is closed
	// (buffer discarded). This prevents early notifications from being lost.
	channelGate := newChannelGate()
	transport = &channelTransport{inner: transport, name: name, gate: channelGate, reg: r}

	client := mcp.NewClient(
		&mcp.Implementation{
			Name:    "braid",
			Version: version.Version,
			Title:   "Braid",
		},
		&mcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				r.broker.Publish(pubsub.UpdatedEvent, Event{
					Type: EventToolsListChanged,
					Name: name,
				})
			},
			PromptListChangedHandler: func(context.Context, *mcp.PromptListChangedRequest) {
				r.broker.Publish(pubsub.UpdatedEvent, Event{
					Type: EventPromptsListChanged,
					Name: name,
				})
			},
			ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) {
				r.broker.Publish(pubsub.UpdatedEvent, Event{
					Type: EventResourcesListChanged,
					Name: name,
				})
			},
			LoggingMessageHandler: func(ctx context.Context, req *mcp.LoggingMessageRequest) {
				level := parseLevel(string(req.Params.Level))
				slog.Log(ctx, level, "MCP log", "name", name, "logger", req.Params.Logger, "data", req.Params.Data)
			},
		},
	)

	session, err := client.Connect(mcpCtx, transport, nil)
	if err != nil {
		err = maybeStdioErr(err, transport)
		if oauthHandler != nil {
			r.detachAuth(name, owner, oauthHandler).Close()
		}
		if closeIdle := closeIdleTransport(transport); closeIdle != nil {
			closeIdle()
		}
		cancel()
		cancelTimer.Stop()
		return nil, err
	}

	cancelTimer.Stop()
	slog.Debug("MCP client initialized", "name", name)

	// Resolve the channel gate: open only for a server that both declares
	// the claude/channel capability and was opted in via --channels.
	// Otherwise close it (fail closed). Resolving drains buffered messages
	// that arrived during negotiation so a fast server does not lose early
	// events.
	if channelOptIn && hasChannelCapability(session.InitializeResult()) {
		buffered := channelGate.resolve(true)
		for _, raw := range buffered {
			r.publishChannelMessage(mcpCtx, name, raw)
		}
		slog.Info("MCP channel enabled", "name", name, "buffered", len(buffered))
	} else {
		channelGate.resolve(false)
	}

	var auth *ownedAuthHandler
	if oauthHandler != nil {
		r.publishMu.Lock()
		publication, ok := r.authURLs.Get(name)
		if ok && publication.gen == owner.gen && publication.attempt == owner.seq && publication.auth.handler == oauthHandler {
			auth = publication.auth
		}
		r.publishMu.Unlock()
		if auth == nil {
			cancel()
			_ = session.Close()
			return nil, context.Canceled
		}
	}
	return &ClientSession{
		ClientSession: session,
		cancel:        cancel,
		auth:          auth,
		closeIdle:     closeIdleTransport(transport),
	}, nil
}

// maybeStdioErr if a stdio mcp prints an error in non-json format, it'll fail
// to parse, and the cli will then close it, causing the EOF error.
// so, if we got an EOF err, and the transport is STDIO, we try to exec it
// again with a timeout and collect the output so we can add details to the
// error.
// this happens particularly when starting things with npx, e.g. if node can't
// be found or some other error like that.
func maybeStdioErr(err error, transport mcp.Transport) error {
	if !errors.Is(err, io.EOF) {
		return err
	}
	ct, ok := transport.(*mcp.CommandTransport)
	if !ok {
		return err
	}
	if err2 := stdioCheck(ct.Command); err2 != nil {
		err = errors.Join(err, err2)
	}
	return err
}

func maybeTimeoutErr(err error, timeout time.Duration) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s", timeout)
	}
	return err
}

func (r *Registry) oauthSetup(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, gen, attempt uint64, resolver config.VariableResolver, url string) (*mcpoauth.Handler, error) {
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
		r.persistOAuthToken(ctx, cfg, name, owner, tok)
	}, mcpoauth.IsInteractive(ctx), m.OAuthCallbackPort)
	if err != nil {
		return nil, fmt.Errorf("failed to create OAuth handler for mcp %q: %w", name, err)
	}
	r.publishMu.Lock()
	if r.currentGen(name) != gen {
		r.publishMu.Unlock()
		h.Close()
		return nil, context.Canceled
	}
	if r.closing || r.owners[name] != (attemptID{gen: gen, seq: attempt}) {
		r.publishMu.Unlock()
		h.Close()
		return nil, context.Canceled
	}
	owned := newOwnedAuthHandler(h)
	old, hadOld := r.authURLs.Get(name)
	r.authURLs.Set(name, authPublication{auth: owned, gen: gen, attempt: attempt})
	r.publishMu.Unlock()
	if hadOld && old.auth != owned {
		old.auth.Close()
	}
	return h, nil
}

func (r *Registry) createTransport(ctx context.Context, m config.MCPConfig, resolver config.VariableResolver) (mcp.Transport, *mcpoauth.Handler, error) {
	const name = "test"
	return r.createTransportFor(ctx, nil, name, m, r.currentGen(name), r.authAttempt.Add(1), resolver)
}

func (r *Registry) buildHTTPTransport(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, gen, attempt uint64, resolver config.VariableResolver) (string, http.RoundTripper, *mcpoauth.Handler, error) {
	url, err := m.ResolvedURL(resolver)
	if err != nil {
		return "", nil, nil, err
	}
	if strings.TrimSpace(url) == "" {
		return "", nil, nil, fmt.Errorf("mcp %s config requires a non-empty 'url' field", m.Type)
	}
	headers, err := m.ResolvedHeaders(resolver)
	if err != nil {
		return "", nil, nil, err
	}
	transport := http.RoundTripper(&headerRoundTripper{headers: headers, base: newOwnedHTTPTransport()})
	var oauthHandler *mcpoauth.Handler
	if m.OAuth {
		oauthHandler, err = r.oauthSetup(ctx, cfg, name, m, gen, attempt, resolver, url)
		if err != nil {
			return "", nil, nil, err
		}
		transport = newOAuthRoundTripper(oauthHandler, transport)
	}
	return url, transport, oauthHandler, nil
}

func (r *Registry) createTransportFor(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, gen, attempt uint64, resolver config.VariableResolver) (mcp.Transport, *mcpoauth.Handler, error) {
	switch m.Type {
	case config.MCPStdio:
		command, err := resolver.ResolveValue(m.Command)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid mcp command: %w", err)
		}
		if strings.TrimSpace(command) == "" {
			return nil, nil, fmt.Errorf("mcp stdio config requires a non-empty 'command' field")
		}
		args, err := m.ResolvedArgs(resolver)
		if err != nil {
			return nil, nil, err
		}
		envs, err := m.ResolvedEnv(resolver)
		if err != nil {
			return nil, nil, err
		}
		cmd := exec.CommandContext(ctx, home.Long(command), args...)
		cmd.Env = append(os.Environ(), envs...)
		// Run the child in its own process group and kill the whole group when
		// the session context is cancelled. A stdio server often spawns its own
		// children (signal-mcp launches signal-cli); os/exec's default
		// cancellation kills only the direct child, orphaning the rest with
		// PPID 1 — production accumulated 15+ such zombies over two days.
		configureStdioProcess(cmd)
		return &mcp.CommandTransport{
			Command: cmd,
		}, nil, nil
	case config.MCPHttp:
		url, transport, oauthHandler, err := r.buildHTTPTransport(ctx, cfg, name, m, gen, attempt, resolver)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: transport}}, oauthHandler, nil
	case config.MCPSSE:
		url, transport, oauthHandler, err := r.buildHTTPTransport(ctx, cfg, name, m, gen, attempt, resolver)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.SSEClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: transport}}, oauthHandler, nil
	default:
		return nil, nil, fmt.Errorf("unsupported mcp type: %s", m.Type)
	}
}

type headerRoundTripper struct {
	headers map[string]string
	base    http.RoundTripper
}

type ownedHTTPTransport struct{ *http.Transport }

func newOwnedHTTPTransport() *ownedHTTPTransport {
	return &ownedHTTPTransport{Transport: http.DefaultTransport.(*http.Transport).Clone()}
}

func (t *ownedHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.Transport.RoundTrip(req)
}

func closeIdleTransport(t mcp.Transport) func() {
	var rt http.RoundTripper
	switch v := t.(type) {
	case *mcp.StreamableClientTransport:
		rt = v.HTTPClient.Transport
	case *mcp.SSEClientTransport:
		rt = v.HTTPClient.Transport
	}
	for {
		switch v := rt.(type) {
		case *oauthRoundTripper:
			rt = v.base
		case *headerRoundTripper:
			rt = v.base
		case interface{ CloseIdleConnections() }:
			return v.CloseIdleConnections
		default:
			return nil
		}
	}
}

func (rt headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	for k, v := range rt.headers {
		if !strings.EqualFold(k, "Authorization") || clone.Header.Get("Authorization") == "" {
			clone.Header.Set(k, v)
		}
	}
	return rt.base.RoundTrip(clone)
}

// oauthRoundTripper wraps an HTTP transport with OAuth bearer token
// injection and 401-triggered authorization. Used for SSE transports
// that don't support the SDK's OAuthHandler natively. Based on Bruno
// Krugel's implementation from PR #3396.
type oauthRoundTripper struct {
	base    http.RoundTripper
	handler auth.OAuthHandler
}

func newOAuthRoundTripper(handler auth.OAuthHandler, base http.RoundTripper) *oauthRoundTripper {
	return &oauthRoundTripper{base: base, handler: handler}
}

func (rt *oauthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	request := cloneRequest(req)
	resp, err := rt.doRequestWithToken(request)
	if err != nil || (resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden) {
		return resp, err
	}
	// Handlers may inspect or close the failed response. Wrap the body so the
	// transport can guarantee closure without closing it twice.
	resp.Body = &onceCloseReadCloser{ReadCloser: resp.Body}
	defer resp.Body.Close()
	if err := rt.handler.Authorize(req.Context(), request, resp); err != nil {
		return nil, fmt.Errorf("oauth authorize: %w", err)
	}
	if req.Body != nil && req.GetBody == nil {
		return nil, errors.New("cannot retry OAuth request with a non-replayable body")
	}
	retry := cloneRequest(req)
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("reopen OAuth retry body: %w", err)
		}
		retry.Body = body
	}
	return rt.doRequestWithToken(retry)
}

type onceCloseReadCloser struct {
	io.ReadCloser
	once sync.Once
}

func (c *onceCloseReadCloser) Close() error {
	var err error
	c.once.Do(func() { err = c.ReadCloser.Close() })
	return err
}

func cloneRequest(req *http.Request) *http.Request {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	return clone
}

func (rt *oauthRoundTripper) doRequestWithToken(req *http.Request) (*http.Response, error) {
	ts, err := rt.handler.TokenSource(req.Context())
	if err != nil {
		closeRequestBody(req)
		return nil, fmt.Errorf("oauth token source: %w", err)
	}
	if ts != nil {
		token, err := ts.Token()
		if err != nil {
			closeRequestBody(req)
			return nil, fmt.Errorf("oauth token: %w", err)
		}
		if token != nil {
			req.Header.Set("Authorization", "Bearer "+token.AccessToken)
		}
	}
	return rt.base.RoundTrip(req)
}

func closeRequestBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

func mcpTimeout(m config.MCPConfig) time.Duration {
	if m.Timeout > 0 {
		return time.Duration(m.Timeout) * time.Second
	}
	// OAuth flows require user interaction in a browser, so use a
	// generous default to avoid timing out mid-auth.
	if m.OAuth {
		return 30 * time.Second
	}
	return 10 * time.Second
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
func (r *Registry) clearOAuthToken(cfg *config.ConfigStore, name string, owner attemptID, expected *oauth.Token) {
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

// clearMCPData removes a stale MCP server's tools, prompts,
// resources, and auth handlers from global state so they are not
// served to the agent.
func (r *Registry) clearCatalog(name string) {
	r.catalogMu.Lock()
	r.allTools.Del(name)
	r.allPrompts.Del(name)
	r.allResources.Del(name)
	r.catalogChanged()
	r.catalogMu.Unlock()
}

func stdioCheck(old *exec.Cmd) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := slices.Clone(old.Args)
	if len(args) > 0 {
		args = args[1:]
	}
	cmd := exec.CommandContext(ctx, old.Path, args...)
	cmd.Env = slices.Clone(old.Env)
	cmd.Dir = old.Dir
	configureStdioProcess(cmd)
	out, err := cmd.CombinedOutput()
	if err == nil || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil
	}
	return fmt.Errorf("%w: %s", err, string(out))
}
