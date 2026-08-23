package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	return &r.serverLock(name).renew
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
	r.clearCatalog(name)
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

func maybeTimeoutErr(err error, timeout time.Duration) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s", timeout)
	}
	return err
}
