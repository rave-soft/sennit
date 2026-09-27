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

// clientCountGraceTimeout is clientCountGraceRegressionTimeout, widened a
// little under -race for the same reason raceWait widens other budgets --
// but capped well under the 10s production grace it regresses against
// (see clientCountGraceRegressionTimeout's own doc comment), unlike
// raceWait's generic 30s floor: a floor at or above 10s would let the
// exact bug this file pins (ClientCount only dropping once the grace
// timer fires) pass silently under -race.
func clientCountGraceTimeout() time.Duration {
	if raceDetectorEnabled {
		return 4 * time.Second
	}
	return clientCountGraceRegressionTimeout
}

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
	require.Eventually(t, func() bool { return srv.ClientCount() == 1 }, raceWait(time.Second), 5*time.Millisecond,
		"expected the connected client's open Subscribe stream to count")

	client.Shutdown()
	require.Eventually(t, func() bool { return srv.ClientCount() == 0 }, clientCountGraceTimeout(), 5*time.Millisecond,
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

	require.Eventually(t, func() bool { return srv.ClientCount() == 2 }, raceWait(time.Second), 5*time.Millisecond)

	clientA.Shutdown()
	require.Eventually(t, func() bool { return srv.ClientCount() == 1 }, clientCountGraceTimeout(), 5*time.Millisecond)

	clientB.Shutdown()
	require.Eventually(t, func() bool { return srv.ClientCount() == 0 }, clientCountGraceTimeout(), 5*time.Millisecond)
}
