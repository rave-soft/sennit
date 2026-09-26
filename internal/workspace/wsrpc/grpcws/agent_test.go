// This file exercises grpcws's AgentRunStream and AgentRunShellCommand
// server streams end to end (real gRPC over bufconn) -- CLIENT-SERVER.md
// PR 1.2's acceptance tests for the two remaining streaming methods of
// workspace.Workspace: in-order delivery, the terminal event, a
// synchronous start error's identity, caller cancellation (and that it
// reaches the server-side ctx, per agentServer.AgentRunStream's doc
// comment), and a dead server translating to ErrServerUnreachable rather
// than a channel left open forever.
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

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// ctxReactiveStreamStub is a workspace.Workspace whose AgentRunStream
// mimics AppWorkspace.AgentRunStream's own cancellation contract closely
// enough for wire testing: it never produces anything on its own, but
// delivers exactly one terminal event derived from ctx.Err() once ctx is
// cancelled, then closes its channel -- the shape
// TestAgentRunStream_CallerCancelMidTurn needs to observe both "the client
// gets a terminal context.Canceled event" and "the server-side ctx was
// actually cancelled" (not just abandoned).
type ctxReactiveStreamStub struct {
	*wsrpctest.StubWorkspace
	ctxCh chan context.Context
}

func (s *ctxReactiveStreamStub) AgentRunStream(ctx context.Context, _ string, _ string, _ workspace.AgentRunOptions) (<-chan workspace.AgentRunEvent, error) {
	if s.ctxCh != nil {
		select {
		case s.ctxCh <- ctx:
		default:
		}
	}
	out := make(chan workspace.AgentRunEvent)
	go func() {
		defer close(out)
		<-ctx.Done()
		select {
		case out <- workspace.AgentRunEvent{Done: true, Err: workspace.EncodeError(ctx.Err())}:
		case <-time.After(2 * time.Second):
		}
	}()
	return out, nil
}

func TestAgentRunStream_DeliversEventsInOrderThenCloses(t *testing.T) {
	t.Parallel()

	events := make(chan workspace.AgentRunEvent, 4)
	events <- workspace.AgentRunEvent{Status: "thinking"}
	events <- workspace.AgentRunEvent{TextDelta: "hello "}
	events <- workspace.AgentRunEvent{TextDelta: "there"}
	events <- workspace.AgentRunEvent{Done: true}
	close(events)

	stub := &wsrpctest.StubWorkspace{StreamChan: events}
	client := newServerAndClient(t, stub)

	out, err := client.AgentRunStream(context.Background(), "sess-1", "hi", workspace.AgentRunOptions{})
	require.NoError(t, err)

	var got []workspace.AgentRunEvent
	for ev := range out {
		got = append(got, ev)
	}

	require.Equal(t, []workspace.AgentRunEvent{
		{Status: "thinking"},
		{TextDelta: "hello "},
		{TextDelta: "there"},
		{Done: true},
	}, got)
}

func TestAgentRunStream_SynchronousStartErrorPreservesIdentity(t *testing.T) {
	t.Parallel()

	stub := &wsrpctest.StubWorkspace{StreamErr: workspace.ErrAgentNotInitialized}
	client := newServerAndClient(t, stub)

	out, err := client.AgentRunStream(context.Background(), "sess-1", "hi", workspace.AgentRunOptions{})
	require.Nil(t, out)
	require.Error(t, err)
	require.True(t, errors.Is(err, workspace.ErrAgentNotInitialized),
		"expected ErrAgentNotInitialized, got: %v", err)
}

func TestAgentRunStream_CallerCancelMidTurn_DeliversContextCanceledAndCancelsServerCtx(t *testing.T) {
	t.Parallel()

	ctxCh := make(chan context.Context, 1)
	stub := &ctxReactiveStreamStub{StubWorkspace: &wsrpctest.StubWorkspace{}, ctxCh: ctxCh}
	client := newServerAndClient(t, stub)

	ctx, cancel := context.WithCancel(context.Background())
	out, err := client.AgentRunStream(ctx, "sess-1", "hi", workspace.AgentRunOptions{})
	require.NoError(t, err)

	var serverCtx context.Context
	select {
	case serverCtx = <-ctxCh:
	case <-time.After(5 * time.Second):
		t.Fatal("server-side AgentRunStream was never called")
	}

	cancel()

	select {
	case ev, ok := <-out:
		require.True(t, ok)
		require.True(t, ev.Done)
		require.True(t, errors.Is(workspace.DecodeError(ev.Err), context.Canceled),
			"expected context.Canceled, got: %v", workspace.DecodeError(ev.Err))
	case <-time.After(5 * time.Second):
		t.Fatal("no terminal event delivered after caller cancellation")
	}

	select {
	case _, ok := <-out:
		require.False(t, ok, "channel must close after the terminal event")
	case <-time.After(5 * time.Second):
		t.Fatal("channel was never closed after the terminal event")
	}

	waitFor(t, 5*time.Second, func() bool { return serverCtx.Err() != nil })
}

func TestAgentRunStream_ServerDiesMidTurn_TerminalErrServerUnreachableAndChannelCloses(t *testing.T) {
	t.Parallel()

	events := make(chan workspace.AgentRunEvent, 1)
	events <- workspace.AgentRunEvent{Status: "thinking"}
	// Deliberately never closed and never fed a terminal event: the
	// server dying is what ends this turn, not the workspace itself.

	stub := &wsrpctest.StubWorkspace{StreamChan: events}
	srv, stopHub := grpcws.NewServer(stub)
	lis := bufconn.Listen(bufSize)
	go func() { _ = srv.Serve(lis) }()

	dialer := startServer(t, srv, stopHub)
	client := dialClient(t, dialer)

	out, err := client.AgentRunStream(context.Background(), "sess-1", "hi", workspace.AgentRunOptions{})
	require.NoError(t, err)

	select {
	case ev, ok := <-out:
		require.True(t, ok)
		require.Equal(t, "thinking", ev.Status)
	case <-time.After(5 * time.Second):
		t.Fatal("first event never arrived")
	}

	srv.Stop()

	select {
	case ev, ok := <-out:
		require.True(t, ok)
		require.True(t, ev.Done)
		decoded := workspace.DecodeError(ev.Err)
		require.True(t, errors.Is(decoded, workspace.ErrServerUnreachable),
			"expected ErrServerUnreachable, got: %v", decoded)
	case <-time.After(5 * time.Second):
		t.Fatal("no terminal event delivered after the server died")
	}

	select {
	case _, ok := <-out:
		require.False(t, ok, "channel must close (bounded wait), not stay open forever")
	case <-time.After(5 * time.Second):
		t.Fatal("channel was never closed after the server died")
	}
}

// shellStub is a workspace.Workspace stand-in for AgentRunShellCommand
// tests: it calls onProgress with each of chunks, in order, then either
// blocks (on ctx or block, whichever first) or returns immediately with
// resp/err.
type shellStub struct {
	*wsrpctest.StubWorkspace

	chunks []string
	resp   proto.ShellCommandResponse
	err    error
	block  chan struct{}
	ctxCh  chan context.Context
}

func (s *shellStub) AgentRunShellCommand(ctx context.Context, _ string, _ string, _ int, onProgress func(string), _ bool) (proto.ShellCommandResponse, error) {
	if s.ctxCh != nil {
		select {
		case s.ctxCh <- ctx:
		default:
		}
	}
	for _, c := range s.chunks {
		if onProgress != nil {
			onProgress(c)
		}
	}
	if s.block != nil {
		select {
		case <-ctx.Done():
			return proto.ShellCommandResponse{}, ctx.Err()
		case <-s.block:
		}
	}
	if s.err != nil {
		return proto.ShellCommandResponse{}, s.err
	}
	return s.resp, nil
}

func TestAgentRunShellCommand_ProgressThenResponseInOrder(t *testing.T) {
	t.Parallel()

	stub := &shellStub{
		StubWorkspace: &wsrpctest.StubWorkspace{},
		chunks:        []string{"line one\n", "line two\n"},
		resp:          proto.ShellCommandResponse{Output: "line one\nline two\n", ExitCode: 0},
	}
	client := newServerAndClient(t, stub)

	var mu sync.Mutex
	var got []string
	onProgress := func(chunk string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, chunk)
	}

	resp, err := client.AgentRunShellCommand(context.Background(), "sess-1", "echo", 80, onProgress, false)
	require.NoError(t, err)
	require.Equal(t, stub.resp, resp)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"line one\n", "line two\n"}, got)
}

func TestAgentRunShellCommand_ErrorIdentityPreserved(t *testing.T) {
	t.Parallel()

	stub := &shellStub{StubWorkspace: &wsrpctest.StubWorkspace{}, err: workspace.ErrAgentNotInitialized}
	client := newServerAndClient(t, stub)

	resp, err := client.AgentRunShellCommand(context.Background(), "sess-1", "echo", 80, nil, false)
	require.Zero(t, resp)
	require.True(t, errors.Is(err, workspace.ErrAgentNotInitialized),
		"expected ErrAgentNotInitialized, got: %v", err)
}

func TestAgentRunShellCommand_CallerCancelMidCommandCancelsServerCtx(t *testing.T) {
	t.Parallel()

	ctxCh := make(chan context.Context, 1)
	stub := &shellStub{StubWorkspace: &wsrpctest.StubWorkspace{}, block: make(chan struct{}), ctxCh: ctxCh}
	client := newServerAndClient(t, stub)

	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan error, 1)
	go func() {
		_, err := client.AgentRunShellCommand(ctx, "sess-1", "sleep", 80, nil, false)
		resultCh <- err
	}()

	var serverCtx context.Context
	select {
	case serverCtx = <-ctxCh:
	case <-time.After(5 * time.Second):
		t.Fatal("server-side AgentRunShellCommand was never called")
	}

	cancel()

	select {
	case err := <-resultCh:
		require.True(t, errors.Is(err, context.Canceled), "expected context.Canceled, got: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("AgentRunShellCommand never returned after cancellation")
	}

	waitFor(t, 5*time.Second, func() bool { return serverCtx.Err() != nil })
}

// TestAgentStreams_NoGoroutineLeakAfterCancelOrServerDeath is
// TestNewServer_EventHubNoGoroutineLeak's counterpart for the two streams
// this file adds: cancelling AgentRunStream/AgentRunShellCommand mid-turn,
// and the server dying mid-AgentRunStream, must each leave nothing running
// -- the client-side forwarding goroutine included -- once everything is
// torn down.
func TestAgentStreams_NoGoroutineLeakAfterCancelOrServerDeath(t *testing.T) {
	ignoreBaseline := goleak.IgnoreCurrent()

	inlineDial := func(lis *bufconn.Listener) func(context.Context, string) (net.Conn, error) {
		return func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }
	}

	func() {
		// AgentRunStream, cancelled mid-turn. Everything this closure
		// creates is torn down before it returns, unlike
		// newServerAndClient/startServer (t.Cleanup-based -- those only
		// run at the whole test's end, which would still be "running"
		// when goleak checks in between each scenario below).
		ctxCh := make(chan context.Context, 1)
		stub := &ctxReactiveStreamStub{StubWorkspace: &wsrpctest.StubWorkspace{}, ctxCh: ctxCh}
		srv, stopHub := grpcws.NewServer(stub)
		lis := bufconn.Listen(bufSize)
		go func() { _ = srv.Serve(lis) }()
		conn, err := grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(inlineDial(lis)), grpc.WithTransportCredentials(insecure.NewCredentials()))
		require.NoError(t, err)
		client := grpcws.NewClient(conn)

		ctx, cancel := context.WithCancel(context.Background())
		out, err := client.AgentRunStream(ctx, "sess-1", "hi", workspace.AgentRunOptions{})
		require.NoError(t, err)
		<-ctxCh
		cancel()
		for range out {
		}

		require.NoError(t, conn.Close())
		srv.Stop()
		stopHub()
		require.NoError(t, lis.Close())
	}()

	func() {
		// AgentRunShellCommand, cancelled mid-command.
		ctxCh := make(chan context.Context, 1)
		stub := &shellStub{StubWorkspace: &wsrpctest.StubWorkspace{}, block: make(chan struct{}), ctxCh: ctxCh}
		srv, stopHub := grpcws.NewServer(stub)
		lis := bufconn.Listen(bufSize)
		go func() { _ = srv.Serve(lis) }()
		conn, err := grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(inlineDial(lis)), grpc.WithTransportCredentials(insecure.NewCredentials()))
		require.NoError(t, err)
		client := grpcws.NewClient(conn)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = client.AgentRunShellCommand(ctx, "sess-1", "sleep", 80, nil, false)
		}()
		<-ctxCh
		cancel()
		<-done

		require.NoError(t, conn.Close())
		srv.Stop()
		stopHub()
		require.NoError(t, lis.Close())
	}()

	func() {
		// AgentRunStream, server dies mid-turn.
		events := make(chan workspace.AgentRunEvent, 1)
		events <- workspace.AgentRunEvent{Status: "thinking"}
		stub := &wsrpctest.StubWorkspace{StreamChan: events}
		srv, stopHub := grpcws.NewServer(stub)
		lis := bufconn.Listen(bufSize)
		go func() { _ = srv.Serve(lis) }()
		conn, err := grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(inlineDial(lis)), grpc.WithTransportCredentials(insecure.NewCredentials()))
		require.NoError(t, err)
		client := grpcws.NewClient(conn)

		out, err := client.AgentRunStream(context.Background(), "sess-1", "hi", workspace.AgentRunOptions{})
		require.NoError(t, err)
		<-out // the one buffered "thinking" status event
		srv.Stop()
		for range out {
		}

		require.NoError(t, conn.Close())
		stopHub()
		require.NoError(t, lis.Close())
	}()

	waitFor(t, 5*time.Second, func() bool {
		return goleak.Find(ignoreBaseline) == nil
	})
}
