package grpcws

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
)

// wantHandleWorkspaceMethods is every method wsrpc.MethodClasses
// classifies H that hands back a workspace.Workspace -- StartOAuth is
// also class H, but its handle is an OAuthFlow, not a workspace.Workspace,
// so it doesn't fit the Handles service's shape and is wired into its own
// OAuth service instead (see oauth.go), deliberately not here. This is the
// set handlesServiceDesc must cover exactly, mirroring
// completeness_test.go's TestServiceDescCoversEveryUCMethod for the
// generated Workspace service: an H method a future change forgets to
// wire into the Handles service fails loudly here instead of silently
// going unimplemented on the client.
func wantHandleWorkspaceMethods() map[string]bool {
	return map[string]bool{"EnterWorktree": true, "ExitWorktree": true, "AttachThread": true}
}

// TestHandlesServiceDescCoversEveryWorkspaceHandleMethod keeps
// completeness_test.go's check honest for the one class it doesn't cover
// (H): every method this test expects must actually still be classified H
// in wsrpc.MethodClasses (or the expectation itself is stale), and
// handlesServiceDesc's Methods (besides ReleaseHandle, which isn't a
// Workspace method at all) must name exactly that set.
func TestHandlesServiceDescCoversEveryWorkspaceHandleMethod(t *testing.T) {
	want := wantHandleWorkspaceMethods()
	for name := range want {
		require.Equal(t, wsrpc.H, wsrpc.MethodClasses[name], "%s must be class H for this test to mean anything", name)
	}

	got := map[string]bool{}
	for _, md := range handlesServiceDesc.Methods {
		if md.MethodName == "ReleaseHandle" {
			continue
		}
		got[md.MethodName] = true
	}
	require.Equal(t, want, got)
}

// TestHandleRegistry_ResolveUnknownHandleReturnsErrWorkspaceGone checks the
// sentinel identity a client relies on to tell "this handle is gone" apart
// from any other NotFound (CLIENT-SERVER.md, PR 1.3).
func TestHandleRegistry_ResolveUnknownHandleReturnsErrWorkspaceGone(t *testing.T) {
	t.Parallel()

	r := newHandleRegistry(0)
	_, err := r.resolve("nonexistent")
	require.ErrorIs(t, err, workspace.ErrWorkspaceGone)

	_, err = r.resolveHub("nonexistent")
	require.ErrorIs(t, err, workspace.ErrWorkspaceGone)
}

// TestHandleRegistry_ReleaseIsIdempotent checks release's own contract
// directly, underneath the wire: a second release of the same handle (or
// releasing one that was never registered) runs the stored release func
// at most once and reports whether it actually found something to
// release.
func TestHandleRegistry_ReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	r := newHandleRegistry(0)
	var calls int
	handle := r.register(nil, func() { calls++ }, "owner-1")

	require.True(t, r.release(handle))
	require.False(t, r.release(handle), "a second release of an already-gone handle finds nothing, but must not panic or re-run release")
	require.False(t, r.release("never-registered"))
	require.Equal(t, 1, calls, "the underlying release func must run exactly once")
}

// TestHandleRegistry_ReleaseByOwnerOnlyTouchesThatOwner checks the
// isolation the two-clients scenario depends on: releasing one owner's
// handles must never touch another owner's.
func TestHandleRegistry_ReleaseByOwnerOnlyTouchesThatOwner(t *testing.T) {
	t.Parallel()

	r := newHandleRegistry(0)
	var aCalls, bCalls int
	ha := r.register(nil, func() { aCalls++ }, "client-a")
	hb := r.register(nil, func() { bCalls++ }, "client-b")

	r.releaseByOwner("client-a")

	require.Equal(t, 1, aCalls)
	require.Equal(t, 0, bCalls)
	_, err := r.resolve(ha)
	require.Error(t, err)
	_, err = r.resolve(hb)
	require.NoError(t, err)
}

// TestLeaseManager_ExpiresOnlyWhenClientStaysIdle checks the state machine
// directly: active calls suppress expiry, and once active drops back to
// zero the grace timer fires and releases that client's handles -- but
// not before.
func TestLeaseManager_ExpiresOnlyWhenClientStaysIdle(t *testing.T) {
	t.Parallel()

	r := newHandleRegistry(0)
	var released atomic.Int32
	handle := r.register(nil, func() { released.Add(1) }, "client-1")

	grace := 20 * time.Millisecond
	lm := newLeaseManager(grace, r, newOAuthFlowRegistry())

	lm.begin("client-1")
	time.Sleep(3 * grace) // active: must not expire while a call is open.
	require.Zero(t, released.Load())
	lm.end("client-1")

	deadline := time.Now().Add(5 * time.Second)
	for released.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.EqualValues(t, 1, released.Load(), "the handle must be released once its owner has gone idle past the grace")
	_, err := r.resolve(handle)
	require.Error(t, err)
}
