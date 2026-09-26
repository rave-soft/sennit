package grpcws_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/rave-soft/sennit/internal/version"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

func TestHello_RoundTrips(t *testing.T) {
	t.Parallel()

	client := newServerAndClient(t, &wsrpctest.StubWorkspace{})

	got, err := client.Hello(context.Background())
	require.NoError(t, err)
	require.Equal(t, grpcws.ProtocolVersion, got.ProtocolVersion)
	require.Equal(t, version.Version, got.Version)
	require.Equal(t, version.Commit, got.BuildID)
}

// TestHealth_RoundTrips checks the standard grpc_health_v1 service NewServer
// wires in reports SERVING for the empty (overall) service name -- proof
// that the health check works over this transport, on its own proto codec,
// without any interference from the "json" codec registered for the
// Workspace/Meta services.
func TestHealth_RoundTrips(t *testing.T) {
	t.Parallel()

	srv := grpcws.NewServer(&wsrpctest.StubWorkspace{})
	dialer := startServer(t, srv)

	conn, err := dialRawConn(t, dialer)
	require.NoError(t, err)

	healthClient := grpc_health_v1.NewHealthClient(conn)
	resp, err := healthClient.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{})
	require.NoError(t, err)
	require.Equal(t, grpc_health_v1.HealthCheckResponse_SERVING, resp.Status)
}
