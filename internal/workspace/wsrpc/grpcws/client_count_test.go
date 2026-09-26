package grpcws_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// clientCountGraceRegressionTimeout bounds how long these tests wait for
// ClientCount to drop after a client disconnects. It is deliberately far
// below productionHandleLeaseGrace (10s, and these tests take that
// default rather than overriding it with WithHandleLeaseGrace): the fix
// this file pins is that a disconnected client stops counting as
// "connected" right away, not once its handle-release grace timer fires.
// Reintroducing the bug (counting every client still in the grace
// window, not just ones with an open RPC/stream) makes ClientCount stay
// at its pre-disconnect value for the whole 10s grace, so this timeout
// alone turns the assertion red.
const clientCountGraceRegressionTimeout = 2 * time.Second

// TestServer_ClientCount covers the accessor the daemon's idle monitor
// polls (CLIENT-SERVER.md, PR 2.1): 0 before anyone connects, 1 while a
// client's Subscribe stream (opened by Connect) is open, and back to 0
// promptly once the client disconnects -- well before its handle-release
// grace period (the production default, left un-overridden here) would
// elapse. See clientCountGraceRegressionTimeout.
func TestServer_ClientCount(t *testing.T) {
	t.Parallel()

	srv, stopHub := grpcws.NewServer(&wsrpctest.StubWorkspace{})
	dialer := startServer(t, srv, stopHub)

	require.Equal(t, 0, srv.ClientCount())

	client := dialClient(t, dialer)
	require.NoError(t, client.Connect(t.Context()))
	require.Eventually(t, func() bool { return srv.ClientCount() == 1 }, time.Second, 5*time.Millisecond,
		"expected the connected client's open Subscribe stream to count")

	client.Shutdown()
	require.Eventually(t, func() bool { return srv.ClientCount() == 0 }, clientCountGraceRegressionTimeout, 5*time.Millisecond,
		"expected the count to drop back to 0 as soon as the client disconnects, not after its handle-release grace period")
}

// TestServer_ClientCountMultipleClients covers more than one client at
// once, since the idle monitor only needs "any," but a count that quietly
// saturated at 1 would be just as wrong as one that never moved.
func TestServer_ClientCountMultipleClients(t *testing.T) {
	t.Parallel()

	srv, stopHub := grpcws.NewServer(&wsrpctest.StubWorkspace{})
	dialer := startServer(t, srv, stopHub)

	clientA := dialClient(t, dialer)
	require.NoError(t, clientA.Connect(context.Background()))
	clientB := dialClient(t, dialer)
	require.NoError(t, clientB.Connect(context.Background()))

	require.Eventually(t, func() bool { return srv.ClientCount() == 2 }, time.Second, 5*time.Millisecond)

	clientA.Shutdown()
	require.Eventually(t, func() bool { return srv.ClientCount() == 1 }, clientCountGraceRegressionTimeout, 5*time.Millisecond)

	clientB.Shutdown()
	require.Eventually(t, func() bool { return srv.ClientCount() == 0 }, clientCountGraceRegressionTimeout, 5*time.Millisecond)
}
