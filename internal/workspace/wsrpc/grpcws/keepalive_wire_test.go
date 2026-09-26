// This file exercises grpcws's gRPC keepalive (CLIENT-SERVER.md, PR 1.3's
// "Уточнено ревью (п. 3)"): a half-open connection -- the remote end
// vanished without a FIN or RST, as a dropped SSH tunnel typically does --
// is found and closed by the server's own keepalive, which releases the
// handles a leaseManager would otherwise pin open forever (an open
// Subscribe stream counts as activity, so nothing else ever would), while
// a running AgentRunStream turn survives it exactly as it survives any
// other dropped connection (1.2c, commit 9e2accf65). A second test checks
// the flip side: a healthy, idle connection is not mistakenly closed.
//
// Both tests are bounded below by two floors grpc-go enforces internally
// and gives no public way to lower (google.golang.org/grpc/internal's
// KeepaliveMinServerPingTime/KeepaliveMinPingTime, unreachable from
// outside the grpc-go module): a server's own ping interval
// (ServerParameters.Time) is silently raised to at least 1s, and a
// client's (ClientParameters.Time) to at least 10s, each logged as a
// WARNING rather than returned as an error. keepaliveTuning's pingTime
// stays above the server's 1s floor so the value this file asks for is
// the value actually in effect; no test here shrinks the *client's* own
// ping interval below its 10s floor, since grpc-go would silently
// override it and the test would then be timing something else entirely.
package grpcws_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// keepaliveTuning picks the server's ping interval/timeout and the
// handle-lease grace this file's tests use: short in the ordinary build
// (but always above grpc-go's own 1s floor on a server's ping interval --
// see this file's doc comment), widened under -race, whose
// instrumentation overhead can otherwise make the server's own keepalive
// monitor (a background goroutine racing this test's assertions) arrive
// late -- see racecheck_off_test.go's own doc comment, which leaseGrace()
// (handles_wire_test.go) already follows for the same reason.
func keepaliveTuning() (pingTime, pingTimeout, leaseGrace time.Duration) {
	if raceDetectorEnabled {
		return 2 * time.Second, time.Second, time.Second
	}
	return 1100 * time.Millisecond, 300 * time.Millisecond, 300 * time.Millisecond
}

// TestKeepalive_HalfOpenConnection_ReleasesHandlesAndTurnSurvives is the
// PR 1.3 acceptance test: a client with an open Subscribe stream and a
// running AgentRunStream turn goes half-open (halfOpenDialer.cut, no
// redial, no manual stop -- unlike TestLeaseGrace_
// ReleasesHandlesOnceClientGoesQuiet's own severableDialer.sever, which
// sends a real half-close and additionally needed the subscription
// stopped by hand to keep gRPC from silently redialing through it). Only
// the server's own keepalive can ever notice this: it is not configured
// by default (see this package's git history before this change), which
// is exactly why the old lease test never simulated a real disconnect.
func TestKeepalive_HalfOpenConnection_ReleasesHandlesAndTurnSurvives(t *testing.T) {
	t.Parallel()

	pingTime, pingTimeout, grace := keepaliveTuning()

	ctxCh := make(chan context.Context, 1)
	evCh := make(chan workspace.AgentRunEvent, 4)
	child := &wsrpctest.StubWorkspace{}
	root := &ctxReactiveStreamStub{
		StubWorkspace: &wsrpctest.StubWorkspace{
			WorktreeWorkspace:  child,
			WorktreeReleased:   make(chan struct{}),
			SubscribeWithReady: make(chan struct{}),
		},
		ctxCh: ctxCh,
		evCh:  evCh,
	}

	srv, stopHub := grpcws.NewServer(root,
		grpcws.WithKeepaliveParams(pingTime, pingTimeout),
		grpcws.WithHandleLeaseGrace(grace),
	)
	lis := bufconn.Listen(bufSize)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		stopHub()
		_ = lis.Close()
	})

	// Deliberately no client-side keepalive here: this test isolates the
	// server's own detection (bullet 1 of the acceptance test), and a
	// client that also pinged would eventually give up on its own end too
	// (Close()ing its half of the shared bufconn pipe, which -- unlike a
	// real dead network -- *would* propagate a clean half-close to the
	// server), which would let this test pass even with the server-side
	// fix reverted. See TestKeepalive_HealthyIdleConnectionSurvives... for
	// the client-side keepalive's own contract.
	dialer := &halfOpenDialer{lis: lis}
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer.dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := grpcws.NewClient(conn)

	stopSub := client.SubscribeWith(func(any) {})
	t.Cleanup(stopSub)
	select {
	case <-root.SubscribeWithReady:
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe never reached the server")
	}

	_, _, err = client.EnterWorktree(context.Background(), "feature")
	require.NoError(t, err)

	out, err := client.AgentRunStream(context.Background(), "sess-1", "hi", workspace.AgentRunOptions{})
	require.NoError(t, err)

	var serverCtx context.Context
	select {
	case serverCtx = <-ctxCh:
	case <-time.After(5 * time.Second):
		t.Fatal("server-side AgentRunStream was never called")
	}

	evCh <- workspace.AgentRunEvent{Status: "thinking"}
	select {
	case ev, ok := <-out:
		require.True(t, ok)
		require.Equal(t, "thinking", ev.Status)
	case <-time.After(5 * time.Second):
		t.Fatal("first event never arrived before the cut")
	}

	// The network vanishes: nothing this client does from here reaches
	// the server, and it never dials again.
	dialer.cut()

	select {
	case <-root.WorktreeReleased:
	case <-time.After(pingTime + pingTimeout + grace + 10*time.Second):
		t.Fatal("handle was never released after the connection went half-open")
	}

	// The turn itself must still be running -- same proof
	// TestAgentRunStream_ConnectionDroppedMidTurn_TurnSurvivesNoAgentCancel
	// uses: poll turnCtx.Err() for a while rather than a single immediate
	// check, since streamCtx.Done() firing and this goroutine noticing it
	// are not instantaneous.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		require.NoError(t, serverCtx.Err(), "turnCtx must not be cancelled by a half-open connection")
		time.Sleep(10 * time.Millisecond)
	}

	// Let the stub's run finish cleanly so it doesn't leak.
	evCh <- workspace.AgentRunEvent{Done: true}
	close(evCh)

	root.AgentCancelMu.Lock()
	calls := len(root.AgentCancelCalls)
	root.AgentCancelMu.Unlock()
	require.Zero(t, calls, "a half-open connection must not send AgentCancel")
}

// TestKeepalive_HealthyIdleConnectionSurvivesSeveralPingIntervals is the
// flip side: a connection with no open RPC/stream at all, left idle
// across several of the server's own ping intervals, must not be closed.
// The client dials with grpcws.DefaultClientDialOptions() -- the real
// production defaults (30s/10s), not a shrunk test value -- both because
// grpc-go's own 10s floor on a client's ping interval (this file's doc
// comment) makes shrinking it pointless, and because 30s comfortably
// clears keepaliveMinTime(pingTime) for any pingTime this test picks, so
// this is exactly the configuration ClientDialOptions' own doc comment
// promises never triggers a "too_many_pings" GOAWAY.
func TestKeepalive_HealthyIdleConnectionSurvivesSeveralPingIntervals(t *testing.T) {
	t.Parallel()

	pingTime, pingTimeout, _ := keepaliveTuning()

	root := &wsrpctest.StubWorkspace{}
	srv, stopHub := grpcws.NewServer(root, grpcws.WithKeepaliveParams(pingTime, pingTimeout))
	lis := bufconn.Listen(bufSize)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		stopHub()
		_ = lis.Close()
	})

	dialOpts := append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, grpcws.DefaultClientDialOptions()...)
	conn, err := grpc.NewClient("passthrough:///bufnet", dialOpts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := grpcws.NewClient(conn)

	// grpc.NewClient (unlike the old grpc.Dial) starts idle: nothing
	// actually dials until the first RPC. Force the transport up front and
	// wait for it, or the sleep below would idle a channel that was never
	// connected in the first place -- proving nothing about keepalive
	// either way.
	conn.Connect()
	waitForReady(t, conn, 5*time.Second)

	// Stay genuinely idle -- no calls, no streams -- across several of the
	// server's own ping intervals, the exact condition PermitWithoutStream
	// exists for.
	time.Sleep(3*pingTime + pingTimeout)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = client.GetSession(ctx, "sess-1")
	require.NoError(t, err, "an idle connection with default keepalive settings must not have been closed")
}

// waitForReady blocks until conn reaches connectivity.Ready, or fails t.
func waitForReady(t *testing.T, conn *grpc.ClientConn, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if conn.GetState() == connectivity.Ready {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		conn.WaitForStateChange(ctx, conn.GetState())
		cancel()
	}
	t.Fatalf("connection never became ready (state: %s)", conn.GetState())
}
