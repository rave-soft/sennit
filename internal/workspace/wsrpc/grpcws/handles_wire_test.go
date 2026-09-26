// This file exercises EnterWorktree/ExitWorktree/AttachThread and the
// lease that ties their handles to a client's connection over real gRPC
// (bufconn), the CLIENT-SERVER.md PR 1.3 acceptance tests: a returned
// handle's Client reaches its own child workspace (not the root), its own
// events, its release func runs the child's own release exactly once, a
// released or expired handle answers ErrWorkspaceGone, a client that goes
// quiet has its handles released after the grace period, a client that
// reconnects within it keeps them, and one client's disconnect never
// touches another's handles.
package grpcws_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// TestEnterWorktree_ChildClientTargetsChildWorkspace checks that the
// *Client EnterWorktree hands back really talks to the child workspace
// EnterWorktree returned server-side, not the root -- the red check this
// guards against is resolveRoot's closure staying wired to ws regardless
// of the handle it's given.
func TestEnterWorktree_ChildClientTargetsChildWorkspace(t *testing.T) {
	t.Parallel()

	child := &wsrpctest.StubWorkspace{AgentModelResult: wsrpctest.AgentModelSample}
	root := &wsrpctest.StubWorkspace{WorktreeWorkspace: child}
	client := newServerAndClient(t, root)

	childWS, release, err := client.EnterWorktree(context.Background(), "feature")
	require.NoError(t, err)
	require.Equal(t, "feature", root.GotWorktreeName)
	require.NotNil(t, release)
	t.Cleanup(release)

	childClient, ok := childWS.(*grpcws.Client)
	require.True(t, ok, "EnterWorktree must hand back a *grpcws.Client")
	require.NotEqual(t, "", childClient.Handle(), "the child must be bound to a real, non-root handle")

	got := childClient.AgentModel()
	require.Equal(t, wsrpctest.AgentModelSample, got, "calls on the returned client must reach the child stub, not root")
}

// TestEnterWorktree_EventsArriveOnItsOwnSubscribe checks that Subscribe
// on the handle EnterWorktree returned reaches that child's own event
// hub -- resolved through registry.resolveHub, not the root hub -- and
// never touches the root's own upstream subscription.
func TestEnterWorktree_EventsArriveOnItsOwnSubscribe(t *testing.T) {
	t.Parallel()

	child := &wsrpctest.StubWorkspace{SubscribeWithReady: make(chan struct{})}
	root := &wsrpctest.StubWorkspace{WorktreeWorkspace: child}
	client := newServerAndClient(t, root)

	// The root hub already started eagerly at NewServer time (CLIENT-
	// SERVER.md, PR 1.4a), so root.SubscribeWithCalled is true before
	// anything below runs; the real assertion is that the child's own
	// subscription traffic never bumps the root's call count any further.
	rootCallsBefore := root.SubscribeWithCalls.Load()

	childWS, release, err := client.EnterWorktree(context.Background(), "feature")
	require.NoError(t, err)
	t.Cleanup(release)
	childClient := childWS.(*grpcws.Client)

	var mu sync.Mutex
	var got []any
	stop := childClient.SubscribeWith(func(v any) {
		mu.Lock()
		got = append(got, v)
		mu.Unlock()
	})
	t.Cleanup(stop)

	send := waitSubscribed(t, child, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) > 0
	})
	mu.Lock()
	got = nil
	mu.Unlock()

	send(wsrpctest.SessionEvent)
	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})

	require.Equal(t, rootCallsBefore, root.SubscribeWithCalls.Load(), "the child's own subscription must never reach the root's upstream hub")
}

// TestEnterWorktree_ReleaseCallsChildReleaseOnceThenErrWorkspaceGone checks
// the whole release lifecycle: the release func EnterWorktree returned
// runs the child's own release exactly once (calling it twice must not
// double-run it), and every call against the handle afterward decodes as
// errors.Is(err, workspace.ErrWorkspaceGone).
func TestEnterWorktree_ReleaseCallsChildReleaseOnceThenErrWorkspaceGone(t *testing.T) {
	t.Parallel()

	child := &wsrpctest.StubWorkspace{}
	root := &wsrpctest.StubWorkspace{WorktreeWorkspace: child, WorktreeReleased: make(chan struct{})}
	client := newServerAndClient(t, root)

	childWS, release, err := client.EnterWorktree(context.Background(), "feature")
	require.NoError(t, err)
	childClient := childWS.(*grpcws.Client)

	release()
	select {
	case <-root.WorktreeReleased:
	case <-time.After(5 * time.Second):
		t.Fatal("release never reached the child's own release func")
	}

	// A second release (a caller that calls it twice by mistake, or a
	// race with the lease's own sweep) must not panic or re-run it --
	// there is no second channel close to wait on; not panicking (the
	// test would otherwise fail outright) and the assertions below are
	// what prove it was harmless.
	release()

	_, err = childClient.GetSession(context.Background(), "sess-1")
	require.Error(t, err)
	require.True(t, errors.Is(err, workspace.ErrWorkspaceGone), "expected ErrWorkspaceGone after release, got: %v", err)
}

// TestAttachThread_ReleaseDoesNotStopTheThread checks
// workspace.ThreadController.AttachThread's own contract survives the
// wire: detaching (running the release func AttachThread returned) must
// never shut the thread's workspace down -- it only ever releases the
// *view* (CLIENT-SERVER.md, PR 1.3's build step 1; appws/threads.go's
// AttachThread doc comment on why).
func TestAttachThread_ReleaseDoesNotStopTheThread(t *testing.T) {
	t.Parallel()

	child := &wsrpctest.StubWorkspace{}
	root := &wsrpctest.StubWorkspace{AttachThreadWorkspace: child, AttachThreadReleased: make(chan struct{})}
	client := newServerAndClient(t, root)

	_, release, err := client.AttachThread(context.Background(), "thread-1")
	require.NoError(t, err)
	require.Equal(t, "thread-1", root.GotAttachThreadID)

	release()
	select {
	case <-root.AttachThreadReleased:
	case <-time.After(5 * time.Second):
		t.Fatal("release never reached AttachThread's own release func")
	}
	require.False(t, child.ShutdownCalled, "detaching a thread view must never shut the thread's own workspace down")
}

// dialLeaseClient dials a fresh connection against lis through a
// severableDialer (events_stream_test.go), wraps it as a *grpcws.Client,
// and hands back the connection itself too, so a lease test can build a
// second Client sharing it (e.g. one bound to a handle a first Client on
// the same connection minted).
func dialLeaseClient(t *testing.T, lis *bufconn.Listener, opts ...grpcws.ClientOption) (*grpcws.Client, *severableDialer, *grpc.ClientConn) {
	t.Helper()
	dialer := &severableDialer{lis: lis}
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer.dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return grpcws.NewClient(conn, opts...), dialer, conn
}

// leaseGrace picks the test grace period: short in the ordinary build, but
// widened under -race, whose instrumentation overhead can make a
// reconnect that would easily beat a short grace arrive late -- see
// raceDetectorEnabled's own doc comment. The lease logic under test is the
// same either way; only the margin changes.
func leaseGrace() time.Duration {
	if raceDetectorEnabled {
		return 2 * time.Second
	}
	return 150 * time.Millisecond
}

// TestLeaseGrace_ReleasesHandlesOnceClientGoesQuiet checks the core lease
// behavior: a client with an open Subscribe stream (this test's stand-in
// for "connected", the same way a real Client always keeps one open) that
// gets disconnected has its handles released once the grace period has
// passed with no new call.
//
// client.Shutdown() runs right after severing, not left running: a
// Client's internal event pump (runPump, started once by Connect) would
// otherwise silently redial through the same severableDialer and resume
// the stream on a fresh connection -- exactly the behavior
// TestSubscribe_ReconnectWithoutLoss relies on, but here it would count as
// renewed activity and mask the disconnect this test means to simulate.
// Shutdown also stops the worktree handle's own pump: EnterWorktree's
// returned Client shares this one's parent lifetime (withParentLifeCtx),
// and its release func is never called here on purpose, standing in for a
// caller that vanished (a crash) rather than one that cleaned up -- so
// Shutdown, not the discarded release, is what has to reach it. Ending
// every subscription client-side this way is standing in for the client
// process actually going away, the same as closing its whole connection
// would.
func TestLeaseGrace_ReleasesHandlesOnceClientGoesQuiet(t *testing.T) {
	t.Parallel()

	grace := leaseGrace()
	child := &wsrpctest.StubWorkspace{}
	root := &wsrpctest.StubWorkspace{
		WorktreeWorkspace:  child,
		WorktreeReleased:   make(chan struct{}),
		SubscribeWithReady: make(chan struct{}),
	}
	srv, stopHub := grpcws.NewServer(root, grpcws.WithHandleLeaseGrace(grace))
	lis := bufconn.Listen(bufSize)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		stopHub()
		_ = lis.Close()
	})

	client, dialer, _ := dialLeaseClient(t, lis)
	client.SubscribeWith(func(any) {})
	select {
	case <-root.SubscribeWithReady:
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe never reached the server")
	}

	_, _, err := client.EnterWorktree(context.Background(), "feature")
	require.NoError(t, err)

	dialer.sever()
	client.Shutdown()

	select {
	case <-root.WorktreeReleased:
	case <-time.After(10 * time.Second):
		t.Fatal("handle was never released after the client went quiet past the lease grace")
	}
}

// TestLeaseGrace_ReleasesHandlesOnceClientGoesQuiet_RealDisconnectNoRedial
// is TestLeaseGrace_ReleasesHandlesOnceClientGoesQuiet's real-disconnect
// sibling (CLIENT-SERVER.md, PR 1.3's "Уточнено ревью (п. 3)"): unlike
// that test's severableDialer.sever(), which calls the real Close() and
// so sends a clean half-close through bufconn's own pipe (and needed the
// Subscribe stream stopped by hand, or gRPC would have silently redialed
// through the still-live listener and resumed as if nothing happened),
// this one uses halfOpenDialer (halfopen_test.go): the connection goes
// half-open -- the client never signals anything, ever, and never dials
// again -- with the Subscribe stream simply left running. Only the
// server's own keepalive (grpcws.WithKeepaliveParams) can notice a
// connection like this at all; without it, this handle would stay leased
// forever, since an open stream counts as activity on its own.
func TestLeaseGrace_ReleasesHandlesOnceClientGoesQuiet_RealDisconnectNoRedial(t *testing.T) {
	t.Parallel()

	pingTime, pingTimeout, grace := keepaliveTuning()
	child := &wsrpctest.StubWorkspace{}
	root := &wsrpctest.StubWorkspace{
		WorktreeWorkspace:  child,
		WorktreeReleased:   make(chan struct{}),
		SubscribeWithReady: make(chan struct{}),
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

	// No client-side keepalive: see
	// TestKeepalive_HalfOpenConnection_ReleasesHandlesAndTurnSurvives's
	// doc comment on why one would let this test pass even with the
	// server-side fix reverted.
	dialer := &halfOpenDialer{lis: lis}
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer.dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := grpcws.NewClient(conn)

	stopSub := client.SubscribeWith(func(any) {})
	t.Cleanup(stopSub) // left running until cleanup -- no manual stop, no redial.
	select {
	case <-root.SubscribeWithReady:
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe never reached the server")
	}

	_, _, err = client.EnterWorktree(context.Background(), "feature")
	require.NoError(t, err)

	dialer.cut()

	select {
	case <-root.WorktreeReleased:
	case <-time.After(pingTime + pingTimeout + grace + 10*time.Second):
		t.Fatal("handle was never released after the connection went half-open")
	}
}

// TestLeaseGrace_ReconnectWithinGraceKeepsHandles checks the flip side:
// severing the connection and reconnecting (a fresh connection, the same
// client ID) before the grace period elapses must keep every handle that
// client held valid -- the grace timer armed by the disconnect is
// cancelled by the reconnect's own activity before it ever fires.
func TestLeaseGrace_ReconnectWithinGraceKeepsHandles(t *testing.T) {
	t.Parallel()

	grace := leaseGrace()
	child := &wsrpctest.StubWorkspace{}
	root := &wsrpctest.StubWorkspace{
		WorktreeWorkspace:  child,
		WorktreeReleased:   make(chan struct{}),
		SubscribeWithReady: make(chan struct{}),
	}
	srv, stopHub := grpcws.NewServer(root, grpcws.WithHandleLeaseGrace(grace))
	lis := bufconn.Listen(bufSize)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		stopHub()
		_ = lis.Close()
	})

	client1, dialer1, _ := dialLeaseClient(t, lis)
	stopSub1 := client1.SubscribeWith(func(any) {})
	select {
	case <-root.SubscribeWithReady:
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe never reached the server")
	}

	childWS, _, err := client1.EnterWorktree(context.Background(), "feature")
	require.NoError(t, err)
	childHandle := childWS.(*grpcws.Client).Handle()
	clientID := client1.ClientID()

	dialer1.sever()
	stopSub1() // the connection is already gone; this only frees the goroutine.

	// Reconnect promptly, as the same client ID, and keep a stream open
	// again -- exactly what a real client's own reconnect loop does. This
	// itself races the grace timer armed by the disconnect above; dialing
	// bufconn and opening a stream is expected to land well within grace.
	client2, dialer2, conn2 := dialLeaseClient(t, lis, grpcws.WithClientID(clientID))
	stopSub2 := client2.SubscribeWith(func(any) {})
	t.Cleanup(stopSub2)
	t.Cleanup(dialer2.sever)

	select {
	case <-root.WorktreeReleased:
		t.Fatal("a reconnect within the grace period must not release the client's handles")
	case <-time.After(grace * 3):
	}

	handleClient := grpcws.NewClient(conn2, grpcws.WithHandle(childHandle), grpcws.WithClientID(clientID))
	_, err = handleClient.GetSession(context.Background(), "sess-1")
	require.NoError(t, err, "the handle must still resolve after a reconnect within grace")
}

// TestLeaseGrace_OneClientsDisconnectDoesNotTouchAnothers checks the
// isolation two independent clients depend on: client B disconnecting
// must never release client A's handles.
func TestLeaseGrace_OneClientsDisconnectDoesNotTouchAnothers(t *testing.T) {
	t.Parallel()

	grace := leaseGrace()
	childA := &wsrpctest.StubWorkspace{}
	childB := &wsrpctest.StubWorkspace{}
	root := &wsrpctest.StubWorkspace{
		WorktreeWorkspace:  childA,
		WorktreeReleased:   make(chan struct{}),
		SubscribeWithReady: make(chan struct{}),
	}
	srv, stopHub := grpcws.NewServer(root, grpcws.WithHandleLeaseGrace(grace))
	lis := bufconn.Listen(bufSize)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		stopHub()
		_ = lis.Close()
	})

	clientA, dialerA, _ := dialLeaseClient(t, lis)
	stopSubA := clientA.SubscribeWith(func(any) {})
	t.Cleanup(stopSubA)
	select {
	case <-root.SubscribeWithReady:
	case <-time.After(5 * time.Second):
		t.Fatal("client A's Subscribe never reached the server")
	}
	_, _, err := clientA.EnterWorktree(context.Background(), "feature-a")
	require.NoError(t, err)
	_ = dialerA // client A's own connection stays up for the rest of this test.

	// client B: attach to the (unrelated, from root's perspective) thread
	// path so it mints its own handle too, then disconnect it.
	root.AttachThreadWorkspace = childB
	root.AttachThreadReleased = make(chan struct{})
	clientB, dialerB, _ := dialLeaseClient(t, lis)
	clientB.SubscribeWith(func(any) {})
	_, _, err = clientB.AttachThread(context.Background(), "thread-b")
	require.NoError(t, err)

	dialerB.sever()
	clientB.Shutdown()

	select {
	case <-root.AttachThreadReleased:
	case <-time.After(10 * time.Second):
		t.Fatal("client B's own handle was never released after it went quiet")
	}

	// Give client A's (never-touched) handle every chance it would need
	// to be wrongly released too, then confirm it wasn't.
	select {
	case <-root.WorktreeReleased:
		t.Fatal("client A's handle must not be released by client B's disconnect")
	case <-time.After(grace * 3):
	}
}

// TestNewServer_NonRootHandleNoGoroutineLeak mirrors events_stream_test.go's
// TestNewServer_EventHubNoGoroutineLeak for a handle EnterWorktree minted:
// its own event hub's upstream SubscribeWith goroutine must not survive
// the server stopping, the same as the root's -- NewServer's stop func
// (registry.closeAll, alongside rootHub.close) is what this exercises.
func TestNewServer_NonRootHandleNoGoroutineLeak(t *testing.T) {
	ignoreBaseline := goleak.IgnoreCurrent()

	func() {
		child := &wsrpctest.StubWorkspace{SubscribeWithReady: make(chan struct{})}
		root := &wsrpctest.StubWorkspace{WorktreeWorkspace: child}
		srv, stopHub := grpcws.NewServer(root)
		lis := bufconn.Listen(bufSize)
		go func() { _ = srv.Serve(lis) }()

		conn, err := grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		require.NoError(t, err)
		client := grpcws.NewClient(conn)

		childWS, release, err := client.EnterWorktree(context.Background(), "feature")
		require.NoError(t, err)
		childClient := childWS.(*grpcws.Client)

		stop := childClient.SubscribeWith(func(any) {})
		select {
		case <-child.SubscribeWithReady:
		case <-time.After(5 * time.Second):
			t.Fatal("SubscribeWith was never called")
		}
		stop()
		release()
		childClient.Shutdown()
		client.Shutdown()

		require.NoError(t, conn.Close())
		srv.Stop()
		stopHub()
		require.NoError(t, lis.Close())
	}()

	waitFor(t, 5*time.Second, func() bool {
		return goleak.Find(ignoreBaseline) == nil
	})
}
