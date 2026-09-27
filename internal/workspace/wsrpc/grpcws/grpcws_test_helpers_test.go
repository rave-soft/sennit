package grpcws_test

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

const bufSize = 1 << 20

// raceMultiplier and raceFloor set how far raceWait widens a wait/delivery
// budget under -race: enough to absorb the instrumentation overhead and
// cross-package CPU contention CI's race job runs under (all packages'
// -race suites in parallel, plus SQLite's own race cost -- see AGENTS.md's
// "wall-clock budgets under -race"), never so little that a genuine hang
// still reads as a false pass.
const (
	raceMultiplier = 6
	raceFloor      = 60 * time.Second
)

// raceWait widens a correctness wait's timeout under -race. These budgets
// exist only to turn a hang into a failure, not to assert performance, so
// widening them costs nothing but wall time on an actual hang. Leave a
// budget alone (do not route it through raceWait) when it is itself a
// performance assertion -- see leaseGrace and keepaliveTuning, which widen
// their own margins for the same reason but must stay tight enough to
// still exercise the timing they test.
func raceWait(d time.Duration) time.Duration {
	if !raceDetectorEnabled {
		return d
	}
	if w := d * raceMultiplier; w > raceFloor {
		return w
	}
	return raceFloor
}

// startServer starts srv listening on an in-memory bufconn.Listener and
// returns a dialer for it (grpc.WithContextDialer) plus a cleanup that
// stops the server and closes the listener. t.Cleanup runs the cleanup
// automatically. stopHub is grpcws.NewServer's second return value (or
// nil, when the test's own srv.Stop() call already covers it, as in
// deadClient); it always runs after srv.Stop(), so the root hub's
// SubscribeWith goroutine (started lazily) has nothing left to deliver
// into once it exits.
func startServer(t *testing.T, srv *grpcws.Server, stopHub func()) func(context.Context, string) (net.Conn, error) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(func() {
		srv.Stop()
		if stopHub != nil {
			stopHub()
		}
		_ = lis.Close()
	})
	return func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}
}

// dialRawConn dials dialer over bufconn as a plain *grpc.ClientConn, with
// no content-subtype selected -- for calling a service (grpc_health_v1)
// that isn't part of this package's own "json" codec contract.
func dialRawConn(t *testing.T, dialer func(context.Context, string) (net.Conn, error)) (*grpc.ClientConn, error) {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err == nil {
		t.Cleanup(func() { _ = conn.Close() })
	}
	return conn, err
}

// dialClient dials dialer over bufconn and wraps the connection as a
// *grpcws.Client, cleaning up the connection at test end.
func dialClient(t *testing.T, dialer func(context.Context, string) (net.Conn, error)) *grpcws.Client {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dialing bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return grpcws.NewClient(conn)
}

// newServerAndClient wires ws behind grpcws.NewServer, over bufconn, and
// returns a *grpcws.Client talking to it.
func newServerAndClient(t *testing.T, ws workspace.Workspace) *grpcws.Client {
	t.Helper()
	srv, stopHub := grpcws.NewServer(ws)
	dialer := startServer(t, srv, stopHub)
	return dialClient(t, dialer)
}
