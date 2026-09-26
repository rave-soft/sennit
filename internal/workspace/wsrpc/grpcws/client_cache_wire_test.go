// This file exercises grpcws.Client's cache/pump end to end (real gRPC
// over bufconn) -- CLIENT-SERVER.md, PR 1.4b's own acceptance tests: no
// RPC for a class-C getter once Connect-ed, a server-side state change
// landing in the cache within the publisher's own tick, the pending-
// prompt replay a waiting permission needs, the Resync path, and a child
// Client's own independent cache.
package grpcws_test

import (
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// countingWorkspaceRPCInterceptor counts every unary RPC against the
// Workspace service specifically -- the service a class-C getter would
// have called before PR 1.4b, and must never call again once a Client is
// Connect-ed. Snapshot (the Events service) and any other service are
// deliberately not counted: Connect itself is expected to call Snapshot.
func countingWorkspaceRPCInterceptor(count *atomic.Int64) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if strings.HasPrefix(method, "/sennit.workspace.v1.Workspace/") {
			count.Add(1)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// dialClientWithInterceptor is dialClient (grpcws_test_helpers_test.go)
// plus a unary client interceptor, for tests that need to count RPCs.
func dialClientWithInterceptor(t *testing.T, dialer func(context.Context, string) (net.Conn, error), interceptor grpc.UnaryClientInterceptor) *grpcws.Client {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(interceptor),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return grpcws.NewClient(conn)
}

// TestClientCache_NoRPCForClassCGettersAfterConnect checks the headline
// acceptance test for PR 1.4b: after Connect, every class-C getter answers
// from the cache, with zero Workspace RPCs.
func TestClientCache_NoRPCForClassCGettersAfterConnect(t *testing.T) {
	stub := &wsrpctest.StubWorkspace{
		SubscribeWithReady: make(chan struct{}),
		WorkingDirResult:   "/repo",
		AgentModelResult:   wsrpctest.AgentModelSample,
	}
	srv, stopHub := grpcws.NewServer(stub)
	dialer := startServer(t, srv, stopHub)

	var count atomic.Int64
	client := dialClientWithInterceptor(t, dialer, countingWorkspaceRPCInterceptor(&count))
	require.NoError(t, client.Connect(context.Background()))

	for range 50 {
		require.Equal(t, "/repo", client.WorkingDir())
		require.Equal(t, wsrpctest.AgentModelSample, client.AgentModel())
		require.False(t, client.AgentIsBusy())
	}

	require.Equal(t, int64(0), count.Load(), "a class-C getter must never call the Workspace service")
}

// TestClientCache_StateChangeReachesGetterWithinTick checks that a change
// on the server side (no explicit event of its own -- just the per-hub
// ticker re-checking workspace.ClientState) reaches a Connect-ed Client's
// getter well within the shrunk tick interval, again with zero RPCs.
func TestClientCache_StateChangeReachesGetterWithinTick(t *testing.T) {
	stub := &wsrpctest.StubWorkspace{SubscribeWithReady: make(chan struct{})}
	srv, stopHub := grpcws.NewServer(stub, grpcws.WithClientStateTickInterval(10*time.Millisecond))
	dialer := startServer(t, srv, stopHub)

	var count atomic.Int64
	client := dialClientWithInterceptor(t, dialer, countingWorkspaceRPCInterceptor(&count))
	require.NoError(t, client.Connect(context.Background()))

	select {
	case <-stub.SubscribeWithReady:
	case <-time.After(5 * time.Second):
		t.Fatal("root hub never subscribed")
	}
	require.False(t, client.AgentIsBusy())

	stub.SetAgentIsBusyResult(true)
	waitFor(t, 2*time.Second, client.AgentIsBusy)

	require.Equal(t, int64(0), count.Load(), "the cache must update from the event stream, never a getter-side RPC")
}

// TestClientCache_PendingPermissionReplayedThenNotAfterResolved is
// client_pump_test.go's white-box replay test, this time over a real wire:
// a permission request already pending on the server when a Client
// connects is delivered to a Subscribe attachment, and once resolved
// (PermissionNotification), a newly attached subscriber never sees it.
func TestClientCache_PendingPermissionReplayedThenNotAfterResolved(t *testing.T) {
	perm := permission.PermissionRequest{
		ID: "perm-1", SessionID: "sess-1", ToolCallID: "call-1",
		ToolName: proto.BashToolName, Params: proto.BashPermissionsParams{Command: "echo hi"},
	}
	stub := &wsrpctest.StubWorkspace{
		SubscribeWithReady:   make(chan struct{}),
		PendingPromptsResult: workspace.PendingPrompts{Permissions: []permission.PermissionRequest{perm}},
	}
	client := newServerAndClient(t, stub)
	require.NoError(t, client.Connect(context.Background()))

	var mu sync.Mutex
	var got []any
	stop := client.SubscribeWith(func(v any) {
		mu.Lock()
		got = append(got, v)
		mu.Unlock()
	})
	t.Cleanup(stop)

	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, v := range got {
			if e, ok := v.(pubsub.Event[permission.PermissionRequest]); ok && e.Payload.ToolCallID == "call-1" {
				return true
			}
		}
		return false
	})

	select {
	case <-stub.SubscribeWithReady:
	case <-time.After(5 * time.Second):
		t.Fatal("root hub never subscribed")
	}
	stub.SubscribeWithSend(pubsub.Event[permission.PermissionNotification]{
		Type: pubsub.CreatedEvent, Payload: permission.PermissionNotification{ToolCallID: "call-1", Granted: true},
	})

	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, v := range got {
			if _, ok := v.(pubsub.Event[permission.PermissionNotification]); ok {
				return true
			}
		}
		return false
	})

	var got2 []any
	var mu2 sync.Mutex
	stop2 := client.SubscribeWith(func(v any) {
		mu2.Lock()
		got2 = append(got2, v)
		mu2.Unlock()
	})
	t.Cleanup(stop2)
	time.Sleep(100 * time.Millisecond)

	mu2.Lock()
	defer mu2.Unlock()
	for _, v := range got2 {
		if e, ok := v.(pubsub.Event[permission.PermissionRequest]); ok {
			require.NotEqual(t, "call-1", e.Payload.ToolCallID, "a resolved request must not be replayed to a new subscriber")
		}
	}
}

// TestClientCache_ResyncAppliesFreshSnapshotAndReplaysPending drives the
// Resync path end to end: a tiny event buffer, a severed connection, more
// events published than the buffer can hold, then a reconnect. The
// reconnect must land a ConnectionEvent{Resync}, followed by whatever is
// pending in the fresh snapshot, and the cache (WorkingDir here) must
// reflect the server's current state, not whatever it was before the
// disconnect.
func TestClientCache_ResyncAppliesFreshSnapshotAndReplaysPending(t *testing.T) {
	stub := &wsrpctest.StubWorkspace{SubscribeWithReady: make(chan struct{}), WorkingDirResult: "/before"}
	srv, stopHub := grpcws.NewServer(stub, grpcws.WithEventBufferSize(4))
	lis := bufconn.Listen(bufSize)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		stopHub()
		_ = lis.Close()
	})

	dialer := &severableDialer{lis: lis}
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer.dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := grpcws.NewClient(conn)
	require.NoError(t, client.Connect(context.Background()))

	var mu sync.Mutex
	var events []any
	client.SubscribeWith(func(v any) {
		mu.Lock()
		events = append(events, v)
		mu.Unlock()
	})

	select {
	case <-stub.SubscribeWithReady:
	case <-time.After(5 * time.Second):
		t.Fatal("root hub never subscribed")
	}
	send := stub.SubscribeWithSend

	dialer.sever()
	for range 20 {
		send(wsrpctest.SessionEvent)
	}

	// The server-side world moves on while the client is disconnected.
	perm := permission.PermissionRequest{
		ID: "perm-1", SessionID: "sess-1", ToolCallID: "call-1",
		ToolName: proto.BashToolName, Params: proto.BashPermissionsParams{Command: "echo hi"},
	}
	stub.SetPendingPromptsResult(workspace.PendingPrompts{Permissions: []permission.PermissionRequest{perm}})
	stub.SetWorkingDirResult("/after")

	waitFor(t, 10*time.Second, func() bool { return client.WorkingDir() == "/after" })

	mu.Lock()
	defer mu.Unlock()

	resyncAt := -1
	pendingAt := -1
	for i, v := range events {
		switch e := v.(type) {
		case pubsub.Event[workspace.ConnectionEvent]:
			if e.Payload.State == workspace.ConnectionResync && resyncAt == -1 {
				resyncAt = i
			}
		case pubsub.Event[permission.PermissionRequest]:
			if e.Payload.ToolCallID == "call-1" && pendingAt == -1 {
				pendingAt = i
			}
		}
	}
	require.GreaterOrEqual(t, resyncAt, 0, "expected a ConnectionEvent{Resync}")
	require.GreaterOrEqual(t, pendingAt, 0, "expected the fresh snapshot's pending permission to be replayed")
	require.Greater(t, pendingAt, resyncAt, "the pending prompt replay must follow ConnectionEvent{Resync}")
}

// TestClientCache_ChildFromEnterWorktreeHasItsOwnCache checks that a
// handle Client (EnterWorktree) has a cache seeded from its own handle's
// Snapshot, distinct from the parent's, and that its release func stops
// its pump cleanly.
func TestClientCache_ChildFromEnterWorktreeHasItsOwnCache(t *testing.T) {
	child := &wsrpctest.StubWorkspace{WorkingDirResult: "/child", WorktreeStateResult: workspace.WorktreeState{Name: "feature", Active: true}}
	root := &wsrpctest.StubWorkspace{WorkingDirResult: "/root", WorktreeWorkspace: child}
	client := newServerAndClient(t, root)
	require.NoError(t, client.Connect(context.Background()))
	require.Equal(t, "/root", client.WorkingDir())

	childWS, release, err := client.EnterWorktree(context.Background(), "feature")
	require.NoError(t, err)
	childClient := childWS.(*grpcws.Client)

	require.Equal(t, "/child", childClient.WorkingDir())
	require.True(t, childClient.WorktreeState().Active)
	require.Equal(t, "/root", client.WorkingDir(), "the parent's own cache must be unaffected by the child's")

	release()
}

// TestClientCache_GoleakClean checks that a Connect-ed Client, once
// Shutdown, leaves no goroutine running -- the pump included.
func TestClientCache_GoleakClean(t *testing.T) {
	ignoreBaseline := goleak.IgnoreCurrent()

	func() {
		stub := &wsrpctest.StubWorkspace{SubscribeWithReady: make(chan struct{})}
		srv, stopHub := grpcws.NewServer(stub)
		lis := bufconn.Listen(bufSize)
		go func() { _ = srv.Serve(lis) }()

		conn, err := grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		require.NoError(t, err)
		client := grpcws.NewClient(conn)
		require.NoError(t, client.Connect(context.Background()))
		stop := client.SubscribeWith(func(any) {})
		stop()
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
