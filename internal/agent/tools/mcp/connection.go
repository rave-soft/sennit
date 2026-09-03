package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/version"
)

// errLostOwnership means a concurrent teardown, renewal, or auth flow
// already took over a server before the current attempt could finish; it is
// not a failure, just this attempt losing a race. Callers treat it like a
// cancelled ctx and back off quietly instead of surfacing an error.
var errLostOwnership = errors.New("mcp: lost ownership of connection")

// ErrLostOwnership is the exported face of errLostOwnership for callers
// outside this package (mcp-tools.go's Tool.Run) that need to tell "this
// attempt just lost a race, retry the tool call" apart from a genuine
// connection failure: it is model-recoverable, not a reason to abort the
// whole tool-call batch the way ctx cancellation is.
var ErrLostOwnership = errLostOwnership

// errPingFailed means a keepalive ping to an existing session came back with
// an error, i.e. the connection itself is broken. Unlike errLostOwnership
// this is a genuine failure and must drive the server to StateError.
var errPingFailed = errors.New("mcp: ping failed")

// connectionManager owns per-server connection lifecycle: creating and
// renewing sessions, the initial connect/reconnect/reconcile paths, and
// disabling a server. It does not hold a lock of its own over ownership
// state (owners, sessionOwners, closing, ...); every method that needs an
// atomic "do I still own this server" check reaches through reg and calls
// a Registry method (owns, publishOrClose, teardown, ...) that takes
// publishMu, so publishMu -> catalogMu is always entered by that one
// Registry-owned call path, never independently by connectionManager.
type connectionManager struct {
	reg *Registry

	// newSession creates a client session. It is a seam so tests can exercise
	// renewal concurrency without spawning a real transport.
	newSession    func(ctx context.Context, cfg ConfigProvider, name string, m config.MCPConfig, owner attemptID, resolver config.VariableResolver, channelOptIn bool) (*ClientSession, error)
	ping          func(ctx context.Context, session *ClientSession, timeout time.Duration) error
	listResources func(ctx context.Context, session *ClientSession) ([]*Resource, error)
}

// pingSession pings a session with the server's configured timeout.
func (cm *connectionManager) pingSession(ctx context.Context, s *ClientSession, timeout time.Duration) error {
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.Ping(pingCtx, nil)
}

// closeSessionContext gracefully closes an MCP session. The SDK waits for
// in-flight JSON-RPC handlers before it closes the transport; if that grace
// period expires, interrupt closes the transport directly, which cancels those
// handlers and lets the Close call join instead of leaking its goroutine.
// EOF, context cancellation, and a killed child are ordinary teardown results.
func (cm *connectionManager) closeSessionContext(ctx context.Context, name string, s *ClientSession) {
	done := make(chan error, 1)
	go func() { done <- s.Close() }()

	select {
	case err := <-done:
		cm.logCloseError(name, err)
	case <-ctx.Done():
		if s.interrupt != nil {
			if err := s.interrupt(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) && !processKilledBySignal(err) {
				slog.Warn("Error interrupting MCP session close", "name", name, "error", err)
			}
		}
		cm.logCloseError(name, <-done)
	}
}

func (cm *connectionManager) logCloseError(name string, err error) {
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) && !processKilledBySignal(err) {
		slog.Warn("Error closing MCP session", "name", name, "error", err)
	}
}

// processKilledBySignal reports whether err is the *exec.ExitError produced
// when a stdio MCP server's child process was terminated by a signal rather
// than exiting on its own (closing a session kills the process on
// cancellation) — an expected outcome, not a failure worth warning about.
func processKilledBySignal(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ProcessState != nil && !exitErr.Exited()
}

func (cm *connectionManager) closeSession(name string, s *ClientSession) {
	ctx, cancel := context.WithTimeout(context.Background(), lifecycleCleanupTimeout)
	defer cancel()
	cm.closeSessionContext(ctx, name, s)
}

func (cm *connectionManager) createSession(ctx context.Context, cfg ConfigProvider, name string, m config.MCPConfig, owner attemptID, resolver config.VariableResolver, channelOptIn bool) (*ClientSession, error) {
	timeout := mcpTimeout(m)
	// The session's context must not be ctx itself: for an SSE server the
	// go-sdk binds the long-lived event-stream GET to the context passed to
	// Connect (unlike the streamable transport, which detaches it), so that
	// stream dies the instant ctx is cancelled. ctx here can be a single tool
	// call's context (killed the moment the call returns) or an OAuth flow's
	// context (cancelled by the coordinator the moment auth succeeds) - in
	// neither case should the caller's lifetime become the session's. Building
	// off context.WithoutCancel(ctx) keeps ctx's values (suppressBrowserKey
	// below, tracing) but not its cancellation, so a caller giving up mid-connect
	// no longer aborts it; that is fine because cancelTimer below still bounds
	// the connect with the mcpTimeout-derived deadline, and the session's own
	// lifetime is still ended explicitly via cancel, stored on ClientSession.
	// A caller that gives up mid-connect (e.g. abortAuthFlow on a cancelled
	// interactive auth flow, authcoordinator.go) doesn't rely on this connect
	// observing that cancellation either: it releases ownership and settles
	// terminal state itself, synchronously, so the still-running connect is
	// later discarded by publishOrClose's r.owns check and its
	// close-on-uncommitted guard rather than by context cancellation.
	mcpCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cancelTimer := time.AfterFunc(timeout, cancel)

	transport, oauthHandler, err := cm.reg.createTransportFor(mcpCtx, cfg, name, m, owner.gen, owner.seq, resolver)
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
	var transportConn mcp.Connection
	transport = &channelTransport{
		inner: transport,
		onConnect: func(conn mcp.Connection) {
			transportConn = conn
		},
		name: name,
		gate: channelGate,
		reg:  cm.reg,
	}

	client := mcp.NewClient(
		&mcp.Implementation{
			Name:    brand.Slug,
			Version: version.Version,
			Title:   brand.Name,
		},
		&mcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				cm.reg.broker.Publish(pubsub.UpdatedEvent, Event{
					Type: EventToolsListChanged,
					Name: name,
				})
			},
			PromptListChangedHandler: func(context.Context, *mcp.PromptListChangedRequest) {
				cm.reg.broker.Publish(pubsub.UpdatedEvent, Event{
					Type: EventPromptsListChanged,
					Name: name,
				})
			},
			ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) {
				cm.reg.broker.Publish(pubsub.UpdatedEvent, Event{
					Type: EventResourcesListChanged,
					Name: name,
				})
			},
			// LoggingMessageHandler is deprecated as of MCP protocol
			// 2026-07-28 (SEP-2577: modelcontextprotocol.io/seps/2577-
			// deprecate-roots-sampling-and-logging) but the SDK guarantees
			// it stays functional for at least a twelve-month deprecation
			// window, and there is no replacement API in go-sdk v1.7.0 to
			// migrate to yet. Dropping the handler now would just stop
			// surfacing server-side log messages for no gain, so keep it
			// and revisit when the SDK ships a successor.
			LoggingMessageHandler: func(ctx context.Context, req *mcp.LoggingMessageRequest) { //nolint:staticcheck // SA1019: see comment above
				level := parseLevel(string(req.Params.Level))
				slog.Log(ctx, level, "MCP log", "name", name, "logger", req.Params.Logger, "data", req.Params.Data)
			},
		},
	)

	session, err := client.Connect(mcpCtx, transport, nil)
	if err != nil {
		err = maybeStdioErr(err, transport)
		if oauthHandler != nil {
			cm.reg.detachAuth(name, owner, oauthHandler).Close()
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
			cm.reg.publishChannelMessage(mcpCtx, name, raw)
		}
		slog.Info("MCP channel enabled", "name", name, "buffered", len(buffered))
	} else {
		channelGate.resolve(false)
	}

	var auth *ownedAuthHandler
	if oauthHandler != nil {
		cm.reg.publishMu.Lock()
		publication, ok := cm.reg.authURLs.Get(name)
		if ok && publication.gen == owner.gen && publication.attempt == owner.seq && publication.auth.handler == oauthHandler {
			auth = publication.auth
		}
		cm.reg.publishMu.Unlock()
		if auth == nil {
			cancel()
			_ = session.Close()
			return nil, errLostOwnership
		}
	}
	return &ClientSession{
		ClientSession: session,
		cancel:        cancel,
		auth:          auth,
		closeIdle:     closeIdleTransport(transport),
		interrupt:     transportConn.Close,
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
	ct, ok := unwrapTransport(transport).(*mcp.CommandTransport)
	if !ok {
		return err
	}
	if err2 := stdioCheck(ct.Command); err2 != nil {
		err = errors.Join(err, err2)
	}
	return err
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

// InitializeSingle initializes one MCP client by name.
func (cm *connectionManager) InitializeSingle(ctx context.Context, name string, cfg ConfigProvider) error {
	m, exists := cfg.Config().MCP[name]
	if !exists {
		return fmt.Errorf("mcp '%s' not found in configuration", name)
	}

	if m.Disabled {
		cm.reg.updateState(name, StateDisabled, nil, nil, Counts{})
		slog.Debug("Skipping disabled MCP", "name", name)
		return nil
	}

	owner, err := cm.reg.beginAttempt(name)
	if err != nil {
		return err
	}
	return cm.initClient(ctx, cfg, name, m, owner, cfg.Resolver())
}

// initClient initializes a single MCP client with the given configuration.
// gen is the server generation captured when the attempt was launched; the
// resulting session is only committed if the generation is still current, so
// a config change that restarts the server mid-connect discards this attempt.
func (cm *connectionManager) initClient(ctx context.Context, cfg ConfigProvider, name string, m config.MCPConfig, owner attemptID, resolver config.VariableResolver) error {
	defer cm.reg.detachAuth(name, owner, nil).Close()
	if usesOAuth(m) && !cm.reg.reserveTokenMutation(cfg, name, m, owner) {
		return errLostOwnership
	}
	// OAuth MCPs without a usable cached token require user interaction
	// (browser auth). If a cached token exists with an access token
	// (even if expired), try connecting first so the SDK can attempt a
	// silent refresh. Only defer to the UI if no token is available at
	// all or the token is structurally invalid (empty access token).
	if usesOAuth(m) && !hasUsableToken(m.OAuthToken) {
		if m.OAuthToken != nil {
			cm.reg.clearOAuthToken(cfg, name, owner, m.OAuthToken)
		}
		// updateStateFor clears the catalog and any live session for us on
		// StateNeedsAuth, and the deferred detachAuth above releases the
		// auth handler; no separate clearMCPDataFor call is needed here.
		cm.reg.updateStateFor(name, owner, StateNeedsAuth, nil)
		slog.Info("MCP server requires OAuth authentication", "name", name)
		return nil
	}

	cm.reg.updateStateFor(name, owner, StateStarting, nil, withPending(m))
	err := cm.connectAndRegister(ctx, cfg, name, m, owner, resolver, channelEnabled(cfg.Overrides().EnabledChannels, name))
	if err != nil {
		// If an OAuth MCP fails because the saved token is no longer
		// valid (e.g. refresh token expired or revoked) or no token
		// could be obtained, clear the stale token and prompt the user
		// to re-authenticate instead of leaving the server stuck in
		// StateError.
		if usesOAuth(m) && isOAuthInitErr(err) {
			if m.OAuthToken != nil {
				cm.reg.clearOAuthToken(cfg, name, owner, m.OAuthToken)
			}
			cm.reg.updateStateFor(name, owner, StateNeedsAuth, nil)
			slog.Info("MCP OAuth token is no longer valid, re-authentication required", "name", name, "error", err)
			return nil
		}
		// Setup/listing errors must settle the current attempt; otherwise the UI
		// remains permanently in StateStarting.
		cm.reg.updateStateFor(name, owner, StateError, maybeTimeoutErr(err, mcpTimeout(m)))
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
func (cm *connectionManager) connectAndRegister(ctx context.Context, cfg ConfigProvider, name string, m config.MCPConfig, owner attemptID, resolver config.VariableResolver, channelOptIn bool) error {
	if usesOAuth(m) && !cm.reg.reserveTokenMutation(cfg, name, m, owner) {
		return errLostOwnership
	}
	session, err := cm.createSession(ctx, cfg, name, m, owner, resolver, channelOptIn)
	if err != nil {
		return err
	}
	// publishOrClose is the owns-check-then-commit: it belongs on Registry
	// (the publishMu owner), not here, so the ownership check it opens with
	// and the catalog/session/state write it ends with stay one indivisible
	// critical section.
	return cm.reg.publishOrClose(ctx, name, m, owner, session)
}

// DisableSingle disables and closes a single MCP client by name.
func (cm *connectionManager) DisableSingle(cfg ConfigProvider, name string) error {
	// teardown bumps the generation, invalidating any in-flight connect, and
	// the StateDisabled transition clears the recorded config so a later
	// re-enable (even with an unchanged config) is seen as new and restarts.
	cm.reg.teardown(name)
	cm.reg.updateState(name, StateDisabled, nil, nil, Counts{})
	slog.Info("Disabled mcp client", "name", name)
	return nil
}

// goInitClient launches initClient in a goroutine with panic recovery.
// Shared by Initialize and Reinitialize so the panic-to-state policy
// lives in one place. wg, if non-nil, is Done when the attempt finishes
// (success or failure); Initialize uses it to await startup. The goroutine
// captures the server's generation at launch so a concurrent teardown
// invalidates its result rather than letting it register a stale session.
func (cm *connectionManager) goInitClient(ctx context.Context, cfg ConfigProvider, name string, m config.MCPConfig, wg *sync.WaitGroup) {
	owner, err := cm.reg.beginAttempt(name)
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
			cm.reg.detachAuth(name, owner, nil).Close()
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
				cm.reg.updateStateFor(name, owner, StateError, err)
				slog.Error("Panic in MCP client initialization", "error", err, "name", name)
			}
		}()
		if err := cm.initClient(ctx, cfg, name, m, owner, cfg.Resolver()); err != nil {
			slog.Debug("Failed to initialize MCP client", "name", name, "error", err)
		}
	}()
}

func (cm *connectionManager) getOrRenewClient(ctx context.Context, cfg ConfigProvider, name string) (*ClientSession, error) {
	m := cfg.Config().MCP[name]
	timeout := mcpTimeout(m)

	observedOwner, observedSession, observed := cm.reg.sessionOwner(name)
	var pingErr error
	if observed {
		pingErr = cm.ping(ctx, observedSession, timeout)
		if pingErr == nil {
			cm.reg.publishMu.Lock()
			current := cm.reg.ownsSessionLocked(name, observedOwner, observedSession)
			cm.reg.publishMu.Unlock()
			if current {
				return observedSession, nil
			}
		} else if ctxErr := ctx.Err(); ctxErr != nil {
			// This ping only failed because the caller's own context was
			// already cancelled (e.g. Esc on a queued tool call), not
			// because the session is broken. The symmetric guard below
			// catches this for the lost-ownership/relock path, but that
			// branch is skipped in the common case where the session is
			// still the one we just observed - and without this check
			// we'd fall through to beginRenewal and tear down a healthy
			// session (killing a stdio server's process) just because
			// its owner happened to be cancelled mid-call.
			return nil, ctxErr
		}
	}

	mu := cm.reg.renewLock(name)
	mu.Lock()
	defer mu.Unlock()

	owner, session, ok := cm.reg.sessionOwner(name)
	if !ok {
		return nil, fmt.Errorf("mcp '%s' not available", name)
	}
	if !observed || owner != observedOwner || session != observedSession {
		if err := cm.ping(ctx, session, timeout); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				// The caller's own context was cancelled; that is genuine
				// user cancellation, not a broken connection, so propagate
				// it quietly instead of failing the server.
				return nil, ctxErr
			}
			// This is the current session for the server, and it just
			// failed a keepalive ping: a genuine connection failure, not
			// this attempt losing a race, so it must surface as StateError
			// rather than being swallowed like a lost-ownership condition.
			cm.reg.failStateForSession(name, owner, session, maybeTimeoutErr(err, timeout))
			return nil, errPingFailed
		}
		cm.reg.publishMu.Lock()
		current := cm.reg.ownsSessionLocked(name, owner, session)
		cm.reg.publishMu.Unlock()
		if current {
			return session, nil
		}
		return nil, errLostOwnership
	}

	renewal, ok := cm.reg.beginRenewal(name, observedOwner, observedSession, maybeTimeoutErr(pingErr, timeout))
	if !ok {
		return nil, errLostOwnership
	}
	if usesOAuth(m) && !cm.reg.reserveTokenMutation(cfg, name, m, renewal) {
		return nil, errLostOwnership
	}
	// ctx here can be the lazy renewal path's tool-call context (a broken
	// ping discovered inside a tool call) or the fresh-server init path's
	// long-lived context; either way createSession itself now detaches the
	// session from ctx's cancellation (see the comment there), so this call
	// passes ctx straight through. ctx is still used for ping/list above and
	// publishOrClose below, where a caller cancellation genuinely should be
	// observed.
	newSess, err := cm.newSession(ctx, cfg, name, m, renewal, cfg.Resolver(), channelEnabled(cfg.Overrides().EnabledChannels, name))
	if err != nil {
		cm.reg.clearMCPDataFor(name, renewal)
		if usesOAuth(m) && isOAuthInitErr(err) {
			if m.OAuthToken != nil {
				cm.reg.clearOAuthToken(cfg, name, renewal, m.OAuthToken)
			}
			cm.reg.updateStateFor(name, renewal, StateNeedsAuth, nil)
			slog.Info("MCP OAuth session expired, re-authentication required", "name", name, "error", err)
		} else {
			cm.reg.updateStateFor(name, renewal, StateError, maybeTimeoutErr(err, timeout))
		}
		return nil, err
	}
	if err := cm.reg.publishOrClose(ctx, name, m, renewal, newSess); err != nil {
		// errLostOwnership means a newer attempt already took over the
		// server (teardown, reconnect, or a fresher renewal); this attempt
		// just lost the race and must not clobber whatever state that
		// newer attempt is settling.
		if !errors.Is(err, errLostOwnership) {
			cm.reg.updateStateFor(name, renewal, StateError, err)
		}
		return nil, err
	}
	return newSess, nil
}

// Reinitialize reconciles running MCP servers against the current config.
// Servers added since the last call are started, servers removed are torn
// down, and servers whose config changed are restarted. Unchanged servers
// keep their existing sessions.
//
// Reconciliation is single-flighted per Registry: at
// most one runs at a time. A config write that arrives mid-run just sets a
// dirty flag and returns; the running reconciliation loops once more to
// pick up the newer state. This coalesces a burst of rapid writes into at
// most two reconciles instead of queueing a redundant no-op pass per write,
// while still guaranteeing the final state reflects the latest config.
func (cm *connectionManager) Reinitialize(ctx context.Context, cfg ConfigProvider) {
	cm.reg.reinitMu.Lock()
	if cm.reg.reinitRunning {
		cm.reg.reinitDirty = true
		cm.reg.reinitMu.Unlock()
		return
	}
	cm.reg.reinitRunning = true
	cm.reg.reinitMu.Unlock()

	for {
		cm.reconcileOnce(ctx, cfg)

		cm.reg.reinitMu.Lock()
		if !cm.reg.reinitDirty {
			cm.reg.reinitRunning = false
			cm.reg.reinitMu.Unlock()
			return
		}
		cm.reg.reinitDirty = false
		cm.reg.reinitMu.Unlock()
	}
}

// reconcileOnce applies one reconciliation pass against the current config.
func (cm *connectionManager) reconcileOnce(ctx context.Context, cfg ConfigProvider) {
	current := cfg.Config().MCP
	actions := reconcile(current, cm.reg.states.Copy())
	for name, action := range actions {
		switch action {
		case reinitRemove:
			slog.Info("Removing MCP server no longer in config", "name", name)
			cm.removeServer(name)
		case reinitDisable:
			slog.Info("Disabling MCP server", "name", name)
			if err := cm.DisableSingle(cfg, name); err != nil {
				slog.Warn("Failed to disable MCP server", "name", name, "error", err)
			}
		case reinitStart:
			m := current[name]
			if _, exists := cm.reg.states.Get(name); exists {
				slog.Info("Re-initializing MCP server after config change", "name", name)
			} else {
				slog.Info("Initializing new MCP server after config change", "name", name)
			}
			// teardown bumps the generation, invalidating any in-flight
			// attempt for this server. The StateStarting transition records
			// m as PendingConfig so a subsequent reconcile can tell whether
			// the attempt now in flight matches the latest config.
			cm.reg.teardown(name)
			cm.reg.updateState(name, StateStarting, nil, nil, Counts{}, withPending(m))
			cm.goInitClient(ctx, cfg, name, m, nil)
		}
	}
}

// removeServer fully tears down an MCP server and deletes its state
// entry. Unlike DisableSingle (which keeps the entry as StateDisabled),
// this is for servers that no longer exist in config at all.
func (cm *connectionManager) removeServer(name string) {
	cm.reg.teardown(name)
	cm.reg.states.Del(name)
}

// mcpConfigEqual reports whether two MCPConfig values are equal, ignoring
// the internally-managed OAuthToken field. Field-by-field rather than
// reflect.DeepEqual so the comparison is explicit about what matters.
// TestMCPConfigEqualExhaustive guards against drift: it fails at test
// time if a new field is added to MCPConfig without a decision about
// whether it participates here.
func mcpConfigEqual(a, b config.MCPConfig) bool {
	return a.Command == b.Command &&
		maps.Equal(a.Env, b.Env) &&
		slices.Equal(a.Args, b.Args) &&
		a.Type == b.Type &&
		a.URL == b.URL &&
		a.Disabled == b.Disabled &&
		slices.Equal(a.DisabledTools, b.DisabledTools) &&
		slices.Equal(a.EnabledTools, b.EnabledTools) &&
		a.Timeout == b.Timeout &&
		maps.Equal(a.Headers, b.Headers) &&
		a.OAuth == b.OAuth &&
		a.OAuthClientID == b.OAuthClientID &&
		a.OAuthClientSecret == b.OAuthClientSecret &&
		a.OAuthCallbackPort == b.OAuthCallbackPort
}
