package grpcws_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// TestServer_ClientCount covers the accessor the daemon's idle monitor
// polls (CLIENT-SERVER.md, PR 2.1): 0 before anyone connects, 1 while a
// client's Subscribe stream (opened by Connect) is open, and back to 0
// once the client disconnects and the lease's grace period has elapsed.
func TestServer_ClientCount(t *testing.T) {
	t.Parallel()

	srv, stopHub := grpcws.NewServer(&wsrpctest.StubWorkspace{}, grpcws.WithHandleLeaseGrace(20*time.Millisecond))
	dialer := startServer(t, srv, stopHub)

	require.Equal(t, 0, srv.ClientCount())

	client := dialClient(t, dialer)
	require.NoError(t, client.Connect(t.Context()))
	require.Eventually(t, func() bool { return srv.ClientCount() == 1 }, time.Second, 5*time.Millisecond,
		"expected the connected client's open Subscribe stream to count")

	client.Shutdown()
	require.Eventually(t, func() bool { return srv.ClientCount() == 0 }, time.Second, 5*time.Millisecond,
		"expected the count to drop back to 0 once the client disconnects and its lease grace elapses")
}

// TestServer_ClientCountMultipleClients covers more than one client at
// once, since the idle monitor only needs "any," but a count that quietly
// saturated at 1 would be just as wrong as one that never moved.
func TestServer_ClientCountMultipleClients(t *testing.T) {
	t.Parallel()

	srv, stopHub := grpcws.NewServer(&wsrpctest.StubWorkspace{}, grpcws.WithHandleLeaseGrace(20*time.Millisecond))
	dialer := startServer(t, srv, stopHub)

	clientA := dialClient(t, dialer)
	require.NoError(t, clientA.Connect(context.Background()))
	clientB := dialClient(t, dialer)
	require.NoError(t, clientB.Connect(context.Background()))

	require.Eventually(t, func() bool { return srv.ClientCount() == 2 }, time.Second, 5*time.Millisecond)

	clientA.Shutdown()
	require.Eventually(t, func() bool { return srv.ClientCount() == 1 }, time.Second, 5*time.Millisecond)

	clientB.Shutdown()
	require.Eventually(t, func() bool { return srv.ClientCount() == 0 }, time.Second, 5*time.Millisecond)
}
