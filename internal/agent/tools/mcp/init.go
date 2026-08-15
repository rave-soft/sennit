// Package mcp provides functionality for managing Model Context Protocol (MCP)
// clients within the Braid application.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/rave-soft/braid/internal/config"
	mcpoauth "github.com/rave-soft/braid/internal/oauth/mcp"
)

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
