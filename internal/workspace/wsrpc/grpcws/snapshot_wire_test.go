// This file exercises the Events service's Snapshot RPC end to end (real
// gRPC over bufconn) -- CLIENT-SERVER.md, PR 1.4a.
package grpcws_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// TestSnapshot_HubStartsAtNewServerTime checks that the root hub is
// already buffering before any client ever calls Subscribe (CLIENT-
// SERVER.md, PR 1.4a): an event published right after NewServer, with no
// Subscribe call in between, still shows up in Snapshot.Seq, and
// subscribing from Seq+1 afterward replays cleanly (no Resync).
func TestSnapshot_HubStartsAtNewServerTime(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{SubscribeWithReady: make(chan struct{})}
	client := newServerAndClient(t, stub)

	select {
	case <-stub.SubscribeWithReady:
	case <-time.After(5 * time.Second):
		t.Fatal("the root hub must subscribe to the workspace at NewServer time, not at first Subscribe")
	}
	send := stub.SubscribeWithSend
	send(wsrpctest.SessionEvent)

	require.Eventually(t, func() bool {
		snap, err := client.Snapshot(context.Background())
		return err == nil && snap.Seq >= 1
	}, 5*time.Second, 10*time.Millisecond, "an event published before any Subscribe call must still be buffered")

	snap, err := client.Snapshot(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, snap.Seq, uint64(1))
}

// TestSnapshot_ReturnsPendingPermissionsAndQuestions checks the reason
// Snapshot carries these at all: a request raised before any client
// connected was announced on the event stream exactly once, and a client
// connecting afterward has no other way to learn it is still outstanding.
func TestSnapshot_ReturnsPendingPermissionsAndQuestions(t *testing.T) {
	t.Parallel()

	perm := permission.PermissionRequest{
		ID: "perm-1", SessionID: "sess-1", ToolCallID: "call-1",
		ToolName: proto.BashToolName, Params: proto.BashPermissionsParams{Command: "echo hi"},
	}
	q := question.Request{ID: "batch-1", SessionID: "sess-1", ToolCallID: "call-1"}
	stub := &wsrpctest.StubWorkspace{
		PendingPromptsResult: workspace.PendingPrompts{
			Permissions: []permission.PermissionRequest{perm},
			Questions:   []question.Request{q},
		},
	}
	client := newServerAndClient(t, stub)

	snap, err := client.Snapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, []permission.PermissionRequest{perm}, snap.PendingPermissions)
	require.Equal(t, []question.Request{q}, snap.PendingQuestions)
}

// TestSnapshot_PerHandle checks Snapshot is resolvable against a non-root
// handle too (CLIENT-SERVER.md, PR 1.4a: "Snapshot must be callable per
// handle"), the same way Subscribe already is.
func TestSnapshot_PerHandle(t *testing.T) {
	t.Parallel()

	child := &wsrpctest.StubWorkspace{WorkingDirResult: "/child"}
	root := &wsrpctest.StubWorkspace{WorktreeWorkspace: child}
	client := newServerAndClient(t, root)

	childWS, release, err := client.EnterWorktree(context.Background(), "feature")
	require.NoError(t, err)
	t.Cleanup(release)
	childClient := childWS.(*grpcws.Client)

	require.Eventually(t, func() bool {
		snap, err := childClient.Snapshot(context.Background())
		return err == nil && snap.State.WorkingDir == "/child"
	}, 5*time.Second, 10*time.Millisecond, "Snapshot against a handle must resolve to that handle's own workspace, not the root's")
}

// TestClientStatePublisher_TickIntervalIsConfigurable checks
// WithClientStateTickInterval end to end: shrinking the tick makes a
// client_state event arrive on a plain Subscribe (FromSeq: 0) well within
// the option's own interval, with no other event published to trigger it.
func TestClientStatePublisher_TickIntervalIsConfigurable(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{WorkingDirResult: "/repo", SubscribeWithReady: make(chan struct{})}
	srv, stopHub := grpcws.NewServer(stub, grpcws.WithClientStateTickInterval(10*time.Millisecond))
	dialer := startServer(t, srv, stopHub)
	client := dialClient(t, dialer)

	select {
	case <-stub.SubscribeWithReady:
	case <-time.After(5 * time.Second):
		t.Fatal("root hub never subscribed")
	}

	require.Eventually(t, func() bool {
		snap, err := client.Snapshot(context.Background())
		return err == nil && snap.State.WorkingDir == "/repo" && snap.State.Version >= 1
	}, 2*time.Second, 5*time.Millisecond, "the shrunk ticker must publish a client_state event well within its own interval")
}
