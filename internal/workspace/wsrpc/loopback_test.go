// Package wsrpc_test (external, not wsrpc's own internal test package):
// exercises Loopback end to end against wsrpctest.StubWorkspace -- the
// same stub, and the same representative values, that grpcws's own
// conformance test (grpcws/conformance_test.go) runs through the gRPC
// transport instead, per CLIENT-SERVER.md's PR 1.1 acceptance criteria.
package wsrpc_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
	"github.com/stretchr/testify/require"
)

func TestLoopback_ListMessages_RoundTripsPartsAndArgs(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{ListMessagesResult: wsrpctest.MessageSample}
	lb := wsrpc.NewLoopback(stub)

	got, err := lb.ListMessages(context.Background(), "sess-1")
	require.NoError(t, err)
	require.Equal(t, "sess-1", stub.GotListMessagesSessionID)
	require.Equal(t, wsrpctest.MessageSample, got)
}

func TestLoopback_PermissionGrant_RoundTripsParams(t *testing.T) {
	t.Parallel()

	want := wsrpctest.PermissionRequestSample
	stub := &wsrpctest.StubWorkspace{PermissionGrantOK: true}
	lb := wsrpc.NewLoopback(stub)

	ok, err := lb.PermissionGrant(want)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, stub.GotPermissionGrantArg)
}

func TestLoopback_GetSession_RoundTripsSession(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{}
	lb := wsrpc.NewLoopback(stub)

	got, err := lb.GetSession(context.Background(), "sess-42")
	require.NoError(t, err)
	require.Equal(t, "sess-42", stub.GotGetSessionID)
	require.Equal(t, session.Session{ID: "sess-42", Title: "loopback test session"}, got)
}

// TestLoopback_GetSession_ErrorSurvivesRoundTrip checks that an error
// carrying a sentinel code (session.ErrNotFound, wrapped rather than
// returned bare -- the ordinary shape a real implementation would produce)
// still satisfies errors.Is on the decoded side, per workspace.EncodeError/
// DecodeError's contract.
func TestLoopback_GetSession_ErrorSurvivesRoundTrip(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{GetSessionErr: fmt.Errorf("looking up session: %w", session.ErrNotFound)}
	lb := wsrpc.NewLoopback(stub)

	_, err := lb.GetSession(context.Background(), "missing")
	require.Error(t, err)
	require.True(t, errors.Is(err, session.ErrNotFound), "decoded error should errors.Is session.ErrNotFound, got: %v", err)
}

func TestLoopback_AgentModel_CachedGetterRoundTrips(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{AgentModelResult: wsrpctest.AgentModelSample}
	lb := wsrpc.NewLoopback(stub)

	got := lb.AgentModel()
	require.Equal(t, wsrpctest.AgentModelSample, got)
}

// compile-time sanity: wsrpctest.StubWorkspace satisfies workspace.Workspace
// (via its embedded nil), so it is a legal Loopback/grpcws.Client target in
// both test packages.
var _ workspace.Workspace = (*wsrpctest.StubWorkspace)(nil)
