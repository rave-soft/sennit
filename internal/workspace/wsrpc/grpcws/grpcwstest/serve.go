// Package grpcwstest holds ServeGRPC, the shared bufconn-serve helper
// internal/ui/model's and internal/workspace/appws's SENNIT_TEST_WIRE=grpc
// harnesses both need (CLIENT-SERVER.md, PR 1.6, build step 1). It lives
// under grpcws itself rather than in wsrpc/wsrpctest: wsrpctest is imported
// by several of grpcws's own internal (package grpcws) test files, so it
// cannot import grpcws back without an import cycle at test-binary link
// time -- this package can, since nothing in grpcws imports it.
package grpcwstest

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
)

// bufSize matches the bufconn buffer size grpcws's own tests use (see
// grpcws_test_helpers_test.go's bufSize); there is nothing test-specific
// about the number, so it isn't worth exporting.
const bufSize = 1 << 20

// stateTick is the client-state tick interval ServeGRPC's server uses
// instead of eventHub's 1s production default (see
// grpcws.WithClientStateTickInterval): a test that mutates a stub getter
// and immediately reads it back through the client's cache (CLIENT-
// SERVER.md, PR 1.4b -- "class-C getters over the gRPC client read the
// cache") would otherwise have to wait out, or poll across, a full second
// per assertion. Callers that need to observe "no publish happened yet"
// pass grpcws.WithClientStateTickInterval(longer) of their own through
// opts, which -- like every ServeGRPC option -- is applied after this
// default and so overrides it.
const stateTick = 10 * time.Millisecond

// ServeGRPC serves ws behind grpcws.NewServer over an in-memory bufconn
// listener, dials it, and returns a connected *grpcws.Client -- the
// bufconn-serve helper both harnesses share, rather than each hand-rolling
// its own copy of what grpcws_test_helpers_test.go's newServerAndClient and
// internal/cmd's newGRPCRunClient already duplicate. t.Cleanup shuts the
// client down first (Client.Shutdown drains its own event pump goroutine),
// then stops the gRPC server, then its event hubs, then closes the
// listener -- the same order every existing grpcws test cleans up in, so a
// goleak check run alongside these tests sees nothing left running.
//
// opts are grpcws.ServerOption values applied after the tick-interval
// default above, so a caller can still ask for a different (or disabled)
// tick.
func ServeGRPC(t testing.TB, ws workspace.Workspace, opts ...grpcws.ServerOption) *grpcws.Client {
	t.Helper()

	serverOpts := append([]grpcws.ServerOption{grpcws.WithClientStateTickInterval(stateTick)}, opts...)
	srv, stopHub := grpcws.NewServer(ws, serverOpts...)
	lis := bufconn.Listen(bufSize)
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpcwstest.ServeGRPC: dialing bufconn: %v", err)
	}

	client := grpcws.NewClient(conn)
	t.Cleanup(func() {
		client.Shutdown()
		srv.Stop()
		stopHub()
		_ = conn.Close()
		_ = lis.Close()
	})

	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("grpcwstest.ServeGRPC: client.Connect: %v", err)
	}
	return client
}
