package grpcws

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/workspace"
)

// stubFlow is a minimal workspace.OAuthFlow for oauthFlowRegistry's own
// unit tests -- oauth_wire_test.go exercises the full RPC surface (and
// wsrpctest.StubOAuthFlow, which observes ctx and counts calls) through
// bufconn; these tests check the registry's bookkeeping directly,
// mirroring handles_test.go's TestHandleRegistry_* tests for
// handleRegistry.
type stubFlow struct {
	cancelCalls int
}

func (f *stubFlow) Wait(context.Context) (workspace.OAuthCompletion, error) {
	panic("not used by these tests")
}

func (f *stubFlow) Cancel() { f.cancelCalls++ }

func (f *stubFlow) StartRelay(context.Context, string) (func(), error) {
	return func() {}, nil
}

// TestOAuthFlowRegistry_ResolveUnknownReturnsFalse checks the registry's
// own sentinel-free "not found" contract: unlike handleRegistry.resolve,
// which returns workspace.ErrWorkspaceGone, resolve here is a private
// lookup oauthServer.OAuthWait itself turns into that error -- see
// oauth.go's OAuthWait.
func TestOAuthFlowRegistry_ResolveUnknownReturnsFalse(t *testing.T) {
	t.Parallel()

	r := newOAuthFlowRegistry()
	_, ok := r.resolve("nonexistent")
	require.False(t, ok)
}

// TestOAuthFlowRegistry_ReleaseIsIdempotent checks release's own contract
// directly, underneath the wire: a second release of the same handle (or
// releasing one that was never registered) runs Cancel at most once and
// reports whether it actually found something to release -- mirroring
// TestHandleRegistry_ReleaseIsIdempotent.
func TestOAuthFlowRegistry_ReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	r := newOAuthFlowRegistry()
	flow := &stubFlow{}
	handle := r.register(flow, "owner-1")

	require.True(t, r.release(handle))
	require.False(t, r.release(handle), "a second release of an already-gone handle finds nothing, but must not panic or re-run Cancel")
	require.False(t, r.release("never-registered"))
	require.Equal(t, 1, flow.cancelCalls, "Cancel must run exactly once")
}

// TestOAuthFlowRegistry_ReleaseByOwnerOnlyTouchesThatOwner checks the
// isolation the lease sweep depends on: releasing one owner's flows must
// never touch another owner's -- mirroring
// TestHandleRegistry_ReleaseByOwnerOnlyTouchesThatOwner.
func TestOAuthFlowRegistry_ReleaseByOwnerOnlyTouchesThatOwner(t *testing.T) {
	t.Parallel()

	r := newOAuthFlowRegistry()
	flowA := &stubFlow{}
	flowB := &stubFlow{}
	ha := r.register(flowA, "client-a")
	hb := r.register(flowB, "client-b")

	r.releaseByOwner("client-a")

	require.Equal(t, 1, flowA.cancelCalls)
	require.Equal(t, 0, flowB.cancelCalls)
	_, ok := r.resolve(ha)
	require.False(t, ok)
	_, ok = r.resolve(hb)
	require.True(t, ok)
}

// TestOAuthFlowRegistry_CloseAllReleasesEveryFlow checks NewServer's stop
// func's own use of closeAll: every flow still registered, regardless of
// owner, is cancelled and dropped.
func TestOAuthFlowRegistry_CloseAllReleasesEveryFlow(t *testing.T) {
	t.Parallel()

	r := newOAuthFlowRegistry()
	flowA := &stubFlow{}
	flowB := &stubFlow{}
	r.register(flowA, "client-a")
	r.register(flowB, "client-b")

	r.closeAll()

	require.Equal(t, 1, flowA.cancelCalls)
	require.Equal(t, 1, flowB.cancelCalls)
}
