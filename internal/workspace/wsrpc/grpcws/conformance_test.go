// Package grpcws_test runs the same representative table wsrpc's Loopback
// tests exercise (internal/workspace/wsrpc/loopback_test.go,
// loopback_manual_test.go), this time through a real gRPC round trip over
// bufconn -- Client -> server -> wsrpctest.StubWorkspace -- per
// CLIENT-SERVER.md's PR 1.1 acceptance criteria.
package grpcws_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

func TestConformance_ListMessages_RoundTripsArgsAndResult(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{ListMessagesResult: wsrpctest.MessageSample}
	client := newServerAndClient(t, stub)

	got, err := client.ListMessages(context.Background(), "sess-1")
	require.NoError(t, err)
	require.Equal(t, "sess-1", stub.GotListMessagesSessionID)
	require.Equal(t, wsrpctest.MessageSample, got)
}

func TestConformance_PermissionGrant_RoundTripsParams(t *testing.T) {
	t.Parallel()

	want := wsrpctest.PermissionRequestSample
	stub := &wsrpctest.StubWorkspace{PermissionGrantOK: true}
	client := newServerAndClient(t, stub)

	ok, err := client.PermissionGrant(want)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, stub.GotPermissionGrantArg)
}

// TestConformance_AgentModel_CachedGetterRoundTrips checks a class-C
// getter end to end: it answers from the Client's local cache
// (workspace.ClientState, seeded by Connect's own Snapshot call), never an
// RPC of its own -- see client_getters.go.
func TestConformance_AgentModel_CachedGetterRoundTrips(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{AgentModelResult: wsrpctest.AgentModelSample}
	client := newServerAndClient(t, stub)
	require.NoError(t, client.Connect(context.Background()))

	got := client.AgentModel()
	require.Equal(t, wsrpctest.AgentModelSample, got)
}

// TestConformance_GetSession_ErrorKeepsSentinelIdentity checks that a
// sentinel error (session.ErrNotFound, wrapped the way a real
// implementation would produce it) survives workspace.EncodeError on the
// server, the trailer hop, and workspace.DecodeError on the client with
// its errors.Is identity intact.
func TestConformance_GetSession_ErrorKeepsSentinelIdentity(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{GetSessionErr: fmt.Errorf("looking up session: %w", session.ErrNotFound)}
	client := newServerAndClient(t, stub)

	_, err := client.GetSession(context.Background(), "missing")
	require.Error(t, err)
	require.True(t, errors.Is(err, session.ErrNotFound), "decoded error should errors.Is session.ErrNotFound, got: %v", err)
}

// TestConformance_ContextCancellation_ArrivesAsContextCanceled cancels the
// client's context while GetSession is in flight on the server (blocked on
// GetSessionDelay) and checks the call returns errors.Is(err,
// context.Canceled) -- CLIENT-SERVER.md, PR 1.1's acceptance criteria.
func TestConformance_ContextCancellation_ArrivesAsContextCanceled(t *testing.T) {
	t.Parallel()

	delay := make(chan struct{}) // never closed: GetSession blocks until ctx is done
	stub := &wsrpctest.StubWorkspace{GetSessionDelay: delay}
	client := newServerAndClient(t, stub)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := client.GetSession(ctx, "sess-1")
		errCh <- err
	}()
	cancel()

	err := <-errCh
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled), "expected context.Canceled, got: %v", err)
}
