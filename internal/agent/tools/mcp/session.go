package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	mcpoauth "github.com/rave-soft/sennit/internal/oauth/mcp"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/version"
)

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

func (r *Registry) createSession(ctx context.Context, cfg ConfigProvider, name string, m config.MCPConfig, owner attemptID, resolver config.VariableResolver, channelOptIn bool) (*ClientSession, error) {
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
			Name:    brand.Slug,
			Version: version.Version,
			Title:   brand.Name,
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

func (r *Registry) oauthSetup(ctx context.Context, cfg ConfigProvider, name string, m config.MCPConfig, gen, attempt uint64, resolver config.VariableResolver, url string) (*mcpoauth.Handler, error) {
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
