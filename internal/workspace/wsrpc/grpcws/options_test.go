package grpcws_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// TestWithHandle_SendsNonRootHandle checks that WithHandle actually reaches
// the server as the "sennit-handle" metadata value: PR 1.1's resolve only
// ever recognizes the root handle (""), so any other value must come back
// as the coded NotFound NewServer's resolve returns for it.
func TestWithHandle_SendsNonRootHandle(t *testing.T) {
	t.Parallel()

	srv := grpcws.NewServer(&wsrpctest.StubWorkspace{})
	dialer := startServer(t, srv)
	conn, err := dialRawConn(t, dialer)
	require.NoError(t, err)
	client := grpcws.NewClient(conn, grpcws.WithHandle("some-worktree-handle"))

	_, err = client.GetSession(context.Background(), "sess-1")
	require.Error(t, err, "a non-root handle isn't registered yet (PR 1.3)")
}

// TestWithCallTimeout_BoundsACtxlessCall checks that a Workspace method
// with no context.Context of its own (AgentIsBusy) still respects
// WithCallTimeout: a server that never answers must not hang the caller
// past it.
func TestWithCallTimeout_BoundsACtxlessCall(t *testing.T) {
	t.Parallel()

	srv := grpcws.NewServer(&wsrpctest.StubWorkspace{})
	dialer := startServer(t, srv)
	conn, err := dialRawConn(t, dialer)
	require.NoError(t, err)
	client := grpcws.NewClient(conn, grpcws.WithCallTimeout(time.Nanosecond))

	done := make(chan struct{})
	go func() {
		client.AgentIsBusy() // no error result: must return, not hang
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("AgentIsBusy did not return within the call timeout")
	}
}

// TestWithGRPCServerOptions_AppliesToTheServer checks that
// WithGRPCServerOptions' extra grpc.ServerOption values really reach
// grpc.NewServer, by installing a counting unary interceptor and calling
// through it.
func TestWithGRPCServerOptions_AppliesToTheServer(t *testing.T) {
	t.Parallel()

	var calls int
	interceptor := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		calls++
		return handler(ctx, req)
	}
	srv := grpcws.NewServer(&wsrpctest.StubWorkspace{}, grpcws.WithGRPCServerOptions(grpc.ChainUnaryInterceptor(interceptor)))
	dialer := startServer(t, srv)
	client := dialClient(t, dialer)

	_, _ = client.GetSession(context.Background(), "sess-1")
	require.Equal(t, 1, calls, "WithGRPCServerOptions' interceptor should have run")
}

// TestWithServerHome_OverridesHello checks that WithServerHome's value,
// not os.UserHomeDir(), is what Hello reports.
func TestWithServerHome_OverridesHello(t *testing.T) {
	t.Parallel()

	srv := grpcws.NewServer(&wsrpctest.StubWorkspace{}, grpcws.WithServerHome(func() string { return "/custom/server/home" }))
	dialer := startServer(t, srv)
	client := dialClient(t, dialer)

	got, err := client.Hello(context.Background())
	require.NoError(t, err)
	require.Equal(t, "/custom/server/home", got.ServerHome)
}
