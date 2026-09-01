package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

	// interrupt closes the transport connection directly. ClientSession.Close
	// normally waits for in-flight JSON-RPC handlers before it reaches the
	// transport, so shutdown uses this only after its grace period expires to
	// make that wait cancellable.
	interrupt func() error

	// closeFunc is a test seam for a close that waits for an interrupt. Production
	// sessions leave it nil and close the SDK session directly.
	closeFunc func() error
}

// Close cancels the session context and then closes the underlying session.
func (s *ClientSession) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.auth != nil {
		s.auth.Close()
	}
	if s.closeIdle != nil {
		s.closeIdle()
	}
	if s.closeFunc != nil {
		return s.closeFunc()
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
	// updateStateLocked(..., StateError, ...) below already deletes
	// r.owners[name] as part of retiring the errored attempt, so the
	// previous owner must be captured before that call, not read back from
	// r.owners afterwards (which would already be gone).
	previousOwner := r.owners[name]
	state, _ := r.states.Get(name)
	cleanup := r.updateStateLocked(name, StateError, pingErr, nil, state.Counts)
	// The attempt being replaced can never become the owner again (ownsLocked
	// requires r.owners[name] == owner), so its reservation, if it holds one,
	// would otherwise sit in tokenReservations for the rest of the process's
	// life - only teardown prunes that map, and a healthy server can renew
	// many times without ever tearing down. Deleting the exact (name,
	// old-owner) key under the same lock as the owner swap below retires it
	// atomically, so no other attempt can observe a moment where both the
	// old and new owner appear to hold the reservation.
	delete(r.tokenReservations, tokenWriteOwner{name: name, attempt: previousOwner})
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
	cleanupCtx, cancel := context.WithTimeout(context.Background(), lifecycleCleanupTimeout)
	defer cancel()
	if hasSession {
		r.closeSessionContext(cleanupCtx, name, session)
	}
	if err := waitTokenWrites(cleanupCtx, waiters); err != nil {
		slog.Warn("Timed out waiting for MCP OAuth token writes during teardown", "name", name, "error", err)
	}
}

func maybeTimeoutErr(err error, timeout time.Duration) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s", timeout)
	}
	return err
}
