// Package mcp provides functionality for managing Model Context Protocol (MCP)
// clients within the Sennit application.
package mcp

import (
	"context"
	"log/slog"

	"github.com/rave-soft/sennit/internal/config"
)

// publishOrClose is the owns-check-then-commit at the heart of the
// publishMu design: the "do we still own this server" check and the
// catalog/session/state write that follows it (publishSession) must be one
// indivisible step, or a concurrent teardown could observe and clear an old
// snapshot while this attempt is still in the middle of publishing a newer
// one. That is exactly why this stays a Registry method — the type that
// physically holds publishMu — rather than living on connectionManager
// alongside the createSession call that produces the session it publishes.
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
		return errLostOwnership
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
	if err := validateToolSchemas(tools); err != nil {
		return err
	}
	if !r.owns(name, owner) {
		return errLostOwnership
	}
	r.publishMu.Lock()
	if !r.ownsLocked(name, owner) {
		r.publishMu.Unlock()
		return errLostOwnership
	}
	if session.auth != nil && r.detachAuthLocked(name, owner, session.auth.handler) != session.auth {
		r.publishMu.Unlock()
		return errLostOwnership
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

func (r *Registry) clearMCPDataFor(name string, owner attemptID) {
	r.publishMu.Lock()
	if !r.ownsLocked(name, owner) {
		r.publishMu.Unlock()
		return
	}
	r.clearCatalog(name)
	r.publishMu.Unlock()
	r.detachAuth(name, owner, nil).Close()
}
