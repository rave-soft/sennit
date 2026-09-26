package cmd

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// grpcRunWorkspace extends wsrpctest.StubWorkspace with the handful of
// additional Workspace methods runAgent's non-interactive path needs
// besides AgentRunStream (CreateSession, InitCoderAgentNonInteractive,
// SetCurrentSession) -- StubWorkspace's embedded nil Workspace panics on
// any of these, same as fakeRunWorkspace (run_agent_cancel_test.go) needs
// its own small set for the in-process path. Config()/ServerConfig() are
// deliberately not added: no concrete Workspace implementation this test
// serves over gRPC is meant to answer ServerConfig (see run.go's comment
// on serverConfig's nil result), so this test exercises exactly the path
// every non-AppWorkspace caller of runAgent takes today.
type grpcRunWorkspace struct {
	*wsrpctest.StubWorkspace
	streamCtxCh chan context.Context
}

func (w *grpcRunWorkspace) InitCoderAgentNonInteractive(context.Context) error { return nil }
func (w *grpcRunWorkspace) SetCurrentSession(context.Context, string) error    { return nil }

func (w *grpcRunWorkspace) CreateSession(context.Context, string) (session.Session, error) {
	return session.Session{ID: "sess-1"}, nil
}

// AgentRunStream forwards the ctx it was called with to streamCtxCh
// (best effort) before delegating to the embedded stub's own
// StreamChan/StreamErr-driven behavior, so a test can observe whether
// cancelling the client's ctx reached the server side.
func (w *grpcRunWorkspace) AgentRunStream(ctx context.Context, sessionID, prompt string, opts workspace.AgentRunOptions) (<-chan workspace.AgentRunEvent, error) {
	if w.streamCtxCh != nil {
		select {
		case w.streamCtxCh <- ctx:
		default:
		}
	}
	return w.StubWorkspace.AgentRunStream(ctx, sessionID, prompt, opts)
}

// newGRPCRunClient serves ws behind grpcws.NewServer over an in-memory
// bufconn listener and returns a *grpcws.Client talking to it, tearing
// both down at test end.
func newGRPCRunClient(t *testing.T, ws workspace.Workspace) *grpcws.Client {
	t.Helper()
	srv, stopHub := grpcws.NewServer(ws)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		stopHub()
		_ = lis.Close()
	})

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return grpcws.NewClient(conn)
}

// TestRunAgent_OverGRPC_CleanFinishReturnsNil is
// TestRunAgent_CleanFinish_ReturnsNil driven over a real gRPC round trip
// (bufconn) instead of an in-process fake, proving cmd/run.go's runAgent
// -- the consumer AgentRunStream/AgentRunShellCommand exist for -- gets
// the same successful exit status through grpcws.Client that it gets
// in-process.
func TestRunAgent_OverGRPC_CleanFinishReturnsNil(t *testing.T) {
	events := make(chan workspace.AgentRunEvent, 1)
	events <- workspace.AgentRunEvent{Done: true}
	close(events)

	ws := &grpcRunWorkspace{StubWorkspace: &wsrpctest.StubWorkspace{StreamChan: events}}
	client := newGRPCRunClient(t, ws)

	err := runAgent(context.Background(), client, "hello", "", true, "", false)
	require.NoError(t, err)
}

// TestRunAgent_OverGRPC_CancelMatchesInProcess is
// TestRunAgent_EventChannelClosesWhileCtxCancelled_ReturnsError's gRPC
// counterpart: cancelling runAgent's ctx must report the cancellation
// (not a false success) exactly as it does in-process, and the
// cancellation must reach the server-side call, not just abandon the
// client's own read (see agentServer.AgentRunStream's doc comment on
// why a dropped connection/cancelled ctx stops the turn on both
// transports the same way).
func TestRunAgent_OverGRPC_CancelMatchesInProcess(t *testing.T) {
	streamCtxCh := make(chan context.Context, 1)
	// StreamChan is left nil: the embedded stub's AgentRunStream hands
	// back a nil channel and no error, which blocks runAgent's select
	// forever on the events case (a nil channel is never ready) until
	// ctx cancellation ends the loop -- exactly the scenario under test.
	ws := &grpcRunWorkspace{StubWorkspace: &wsrpctest.StubWorkspace{}, streamCtxCh: streamCtxCh}
	client := newGRPCRunClient(t, ws)

	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan error, 1)
	go func() {
		resultCh <- runAgent(ctx, client, "hello", "", true, "", false)
	}()

	var serverCtx context.Context
	select {
	case serverCtx = <-streamCtxCh:
	case <-time.After(5 * time.Second):
		t.Fatal("server-side AgentRunStream was never called")
	}

	cancel()

	select {
	case err := <-resultCh:
		require.Error(t, err, "a cancelled run must not report success")
		require.True(t, errors.Is(err, context.Canceled), "expected context.Canceled, got: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("runAgent never returned after ctx cancellation")
	}

	deadline := time.Now().Add(5 * time.Second)
	for serverCtx.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.Error(t, serverCtx.Err(), "the server-side call must have been cancelled too")
}
