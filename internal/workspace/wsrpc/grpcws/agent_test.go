// This file exercises grpcws's AgentRunStream and AgentRunShellCommand
// server streams end to end (real gRPC over bufconn) -- CLIENT-SERVER.md
// PR 1.2's acceptance tests for the two remaining streaming methods of
// workspace.Workspace: in-order delivery, the terminal event, a
// synchronous start error's identity, that a dropped connection does NOT
// stop the turn (only an explicit AgentCancel does -- CLIENT-SERVER.md's
// PR 1.2 build step 1.2c, see agentServer.AgentRunStream's doc comment),
// that caller cancellation sends AgentCancel exactly once, and a dead
// server translating to ErrServerUnreachable rather than a channel left
// open forever.
package grpcws_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
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
// produces events only when told to (via evCh), ends on ctx.Done() (a
// safety net; turnCtx no longer becomes Done just because the stream
// disconnects, see agentServer.AgentRunStream's doc comment), or ends
// when AgentCancel is called -- standing in for a real coordinator's
// activeRequests-based cancellation (agent.coordinator.Cancel), which a
// bare ctx can't simulate now that the two are decoupled. Without this,
// a caller-cancel test would leak this goroutine forever: nothing else in
// this fake would ever stop it.
type ctxReactiveStreamStub struct {
	*wsrpctest.StubWorkspace
	ctxCh chan context.Context
	evCh  chan workspace.AgentRunEvent // optional: events to forward before ctx.Done()/cancelCh fires

	cancelCh   chan struct{} // optional: closed by AgentCancel below
	cancelOnce sync.Once
}

// AgentCancel records the call on the embedded stub (for assertions) and,
// if cancelCh is set, closes it to stop AgentRunStream's goroutine --
// simulating what a real coordinator.Cancel(sessionID) does to the run
// it's tracking by session ID, independent of turnCtx.
func (s *ctxReactiveStreamStub) AgentCancel(sessionID string) error {
	_ = s.StubWorkspace.AgentCancel(sessionID)
	if s.cancelCh != nil {
		s.cancelOnce.Do(func() { close(s.cancelCh) })
	}
	return nil
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
		for {
			select {
			case ev, ok := <-s.evCh:
				if !ok {
					return
				}
				select {
				case out <- ev:
				case <-time.After(2 * time.Second):
					return
				}
			case <-ctx.Done():
				select {
				case out <- workspace.AgentRunEvent{Done: true, Err: workspace.EncodeError(ctx.Err())}:
				case <-time.After(2 * time.Second):
				}
				return
			case <-s.cancelCh:
				select {
				case out <- workspace.AgentRunEvent{Done: true, Err: workspace.EncodeError(context.Canceled)}:
				case <-time.After(2 * time.Second):
				}
				return
			}
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

// TestAgentRunStream_CallerCancelMidTurn_SendsAgentCancelAndDeliversContextCanceled
// is CLIENT-SERVER.md PR 1.2 build step 1.2c's acceptance test: caller
// cancellation no longer reaches the server-side turn's own ctx (it's
// detached, see agentServer.AgentRunStream's doc comment) -- what stops
// the turn instead is the client's explicit AgentCancel(sessionID) call,
// which this test observes on the stub directly rather than through the
// turn's ctx.
func TestAgentRunStream_CallerCancelMidTurn_SendsAgentCancelAndDeliversContextCanceled(t *testing.T) {
	t.Parallel()

	ctxCh := make(chan context.Context, 1)
	stub := &ctxReactiveStreamStub{StubWorkspace: &wsrpctest.StubWorkspace{}, ctxCh: ctxCh, cancelCh: make(chan struct{})}
	client := newServerAndClient(t, stub)

	ctx, cancel := context.WithCancel(context.Background())
	out, err := client.AgentRunStream(ctx, "sess-1", "hi", workspace.AgentRunOptions{})
	require.NoError(t, err)

	select {
	case <-ctxCh:
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

	waitFor(t, 5*time.Second, func() bool {
		stub.AgentCancelMu.Lock()
		defer stub.AgentCancelMu.Unlock()
		return len(stub.AgentCancelCalls) == 1
	})
	stub.AgentCancelMu.Lock()
	defer stub.AgentCancelMu.Unlock()
	require.Equal(t, []string{"sess-1"}, stub.AgentCancelCalls)
}

// blockingStartStub is a workspace.Workspace whose AgentRunStream reports
// itself on ctxCh, then blocks until unblock is closed before returning
// (StreamChan, nil) -- letting a test cancel the caller's ctx in the exact
// window between the request reaching the server (SendMsg/CloseSend
// already succeeded) and the Started ack being sent, which is otherwise a
// hard window to hit deterministically.
type blockingStartStub struct {
	*wsrpctest.StubWorkspace
	ctxCh   chan context.Context
	unblock chan struct{}
}

func (s *blockingStartStub) AgentRunStream(ctx context.Context, sessionID, prompt string, opts workspace.AgentRunOptions) (<-chan workspace.AgentRunEvent, error) {
	if s.ctxCh != nil {
		select {
		case s.ctxCh <- ctx:
		default:
		}
	}
	<-s.unblock
	return s.StubWorkspace.AgentRunStream(ctx, sessionID, prompt, opts)
}

// TestAgentRunStream_CallerCancelBeforeStartedAck_StillSendsAgentCancel
// closes the gap TestRunAgent_OverGRPC_CancelMatchesInProcess
// (internal/cmd/run_agent_grpc_test.go) found: cancelling ctx before the
// Started ack arrives fails Client.AgentRunStream synchronously (see its
// own doc comment on the first RecvMsg) instead of going through the
// streaming goroutine's notifyCancel -- a second call site for the exact
// same "caller cancelled" condition, and one that was missing its
// AgentCancel call entirely (CLIENT-SERVER.md PR 1.2 build step 1.2c).
func TestAgentRunStream_CallerCancelBeforeStartedAck_StillSendsAgentCancel(t *testing.T) {
	t.Parallel()

	ctxCh := make(chan context.Context, 1)
	stub := &blockingStartStub{StubWorkspace: &wsrpctest.StubWorkspace{}, ctxCh: ctxCh, unblock: make(chan struct{})}
	client := newServerAndClient(t, stub)

	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan error, 1)
	go func() {
		_, err := client.AgentRunStream(ctx, "sess-1", "hi", workspace.AgentRunOptions{})
		resultCh <- err
	}()

	select {
	case <-ctxCh:
	case <-time.After(5 * time.Second):
		t.Fatal("server-side AgentRunStream was never called")
	}

	cancel()
	close(stub.unblock) // let the server-side call return now that ctx is cancelled

	select {
	case err := <-resultCh:
		require.Error(t, err)
		require.True(t, errors.Is(err, context.Canceled), "expected context.Canceled, got: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("AgentRunStream never returned after cancellation")
	}

	waitFor(t, 5*time.Second, func() bool {
		stub.AgentCancelMu.Lock()
		defer stub.AgentCancelMu.Unlock()
		return len(stub.AgentCancelCalls) == 1
	})
	stub.AgentCancelMu.Lock()
	defer stub.AgentCancelMu.Unlock()
	require.Equal(t, []string{"sess-1"}, stub.AgentCancelCalls)
}

// TestAgentRunStream_ConnectionDroppedMidTurn_TurnSurvivesNoAgentCancel is
// PR 1.2 build step 1.2c's other acceptance test: a dropped connection
// (as opposed to the caller cancelling) must NOT stop the turn and must
// NOT send AgentCancel -- the turn keeps running on the server, detached
// from the stream, exactly as agentServer.AgentRunStream's doc comment
// promises. The dialer refuses every redial attempt after the first
// connection is severed, so gRPC can't silently paper over the drop by
// reconnecting.
func TestAgentRunStream_ConnectionDroppedMidTurn_TurnSurvivesNoAgentCancel(t *testing.T) {
	t.Parallel()

	ctxCh := make(chan context.Context, 1)
	evCh := make(chan workspace.AgentRunEvent, 4)
	stub := &ctxReactiveStreamStub{StubWorkspace: &wsrpctest.StubWorkspace{}, ctxCh: ctxCh, evCh: evCh}

	srv, stopHub := grpcws.NewServer(stub)
	lis := bufconn.Listen(bufSize)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		stopHub()
		_ = lis.Close()
	})

	var dialCount atomic.Int32
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		if dialCount.Add(1) > 1 {
			return nil, errors.New("dial refused: connection severed, no redial in this test")
		}
		return lis.DialContext(ctx)
	}
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := grpcws.NewClient(conn)

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
		t.Fatal("first event never arrived")
	}

	// Sever the connection: the client sees a terminal
	// ErrServerUnreachable event, but the turn keeps running server-side.
	require.NoError(t, conn.Close())

	select {
	case ev, ok := <-out:
		require.True(t, ok)
		require.True(t, ev.Done)
		require.True(t, errors.Is(workspace.DecodeError(ev.Err), workspace.ErrServerUnreachable),
			"expected ErrServerUnreachable, got: %v", workspace.DecodeError(ev.Err))
	case <-time.After(5 * time.Second):
		t.Fatal("no terminal event delivered after the connection dropped")
	}

	// The real proof the turn survives: turnCtx (observed via serverCtx)
	// must still be alive well after the connection dropped, not just
	// "the client got some terminal event" (which a dead server would
	// also produce). Poll instead of a single immediate check since
	// streamCtx.Done() firing and this goroutine noticing are not
	// instantaneous.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		require.NoError(t, serverCtx.Err(), "turnCtx must not be cancelled by a dropped connection")
		time.Sleep(10 * time.Millisecond)
	}

	// Let the stub's run finish cleanly so it doesn't leak: feed it a
	// terminal event and close its channel, same as a real conforming
	// Workspace eventually would (see agentServer.AgentRunStream's doc
	// comment on the server's drain goroutine).
	evCh <- workspace.AgentRunEvent{Done: true}
	close(evCh)

	stub.AgentCancelMu.Lock()
	calls := len(stub.AgentCancelCalls)
	stub.AgentCancelMu.Unlock()
	require.Zero(t, calls, "a dropped connection must not send AgentCancel")
}

func TestAgentRunStream_ServerDiesMidTurn_TerminalErrServerUnreachableAndChannelCloses(t *testing.T) {
	t.Parallel()

	events := make(chan workspace.AgentRunEvent, 1)
	events <- workspace.AgentRunEvent{Status: "thinking"}
	// Not fed a terminal event before the server dies: the server dying
	// is what ends this turn's *observation*, not the workspace itself.
	// It's closed at the end regardless (see below), because the
	// server's own drain goroutine (agentServer.AgentRunStream's doc
	// comment) keeps reading it in the background past that point --
	// same as a conforming Workspace eventually closing it on its own.

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

	close(events)
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
		stub := &ctxReactiveStreamStub{StubWorkspace: &wsrpctest.StubWorkspace{}, ctxCh: ctxCh, cancelCh: make(chan struct{})}
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
		// The server keeps draining events in the background once the
		// stream ends (see agentServer.AgentRunStream's doc comment); a
		// conforming Workspace eventually closes it on its own even
		// though nobody's forwarding it any more, same as this fake does
		// here -- otherwise that drain goroutine would never exit.
		close(events)

		require.NoError(t, conn.Close())
		stopHub()
		require.NoError(t, lis.Close())
	}()

	waitFor(t, 5*time.Second, func() bool {
		return goleak.Find(ignoreBaseline) == nil
	})
}
