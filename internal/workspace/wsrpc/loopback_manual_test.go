package wsrpc_test

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
	"github.com/stretchr/testify/require"
)

// TestLoopback_ManualPassthroughMethods exercises loopback_manual.go's
// hand-written S/H/X methods, now that every one of them (besides
// Shutdown, which never crosses the wire) pushes its data through the
// JSON codec instead of delegating in plain. Each subtest asserts that
// what arrives on the far side is equal to what the stub produced, and
// that an error's identity survives (errors.Is on a sentinel) even
// though it went through workspace.EncodeError/DecodeError and a JSON
// hop to get there.
// Subtests share one stub instance and run sequentially (no
// t.Parallel()): each checks the args the previous subtest's call left
// behind, so running them concurrently would race on the stub's fields.
func TestLoopback_ManualPassthroughMethods(t *testing.T) {
	stub := &wsrpctest.StubWorkspace{}
	lb := wsrpc.NewLoopback(stub)
	ctx := context.Background()

	t.Run("AgentRunShellCommand", func(t *testing.T) {
		stub.ShellResponse = proto.ShellCommandResponse{Output: "hi"}
		got, err := lb.AgentRunShellCommand(ctx, "sess-1", "echo hi", 80, nil, true)
		require.NoError(t, err)
		require.Equal(t, "sess-1", stub.GotShellSessionID)
		require.Equal(t, proto.ShellCommandResponse{Output: "hi"}, got)
	})

	t.Run("AgentRunShellCommand error identity", func(t *testing.T) {
		stub.ShellErr = context.Canceled
		_, err := lb.AgentRunShellCommand(ctx, "sess-1", "echo hi", 80, nil, true)
		require.ErrorIs(t, err, context.Canceled)
		stub.ShellErr = nil
	})

	t.Run("AgentRunStream", func(t *testing.T) {
		ch := make(chan workspace.AgentRunEvent, 1)
		stub.StreamChan = ch
		got, err := lb.AgentRunStream(ctx, "sess-2", "do it", workspace.AgentRunOptions{AutoApprovePermissions: true})
		require.NoError(t, err)
		require.Equal(t, "sess-2", stub.GotStreamSessionID)
		require.True(t, stub.GotStreamOptions.AutoApprovePermissions)

		ch <- workspace.AgentRunEvent{TextDelta: "hi", Status: "thinking"}
		close(ch)
		require.Equal(t, workspace.AgentRunEvent{TextDelta: "hi", Status: "thinking"}, <-got)
		_, open := <-got
		require.False(t, open, "loopback's forwarding channel must close once inner does")
	})

	t.Run("AgentRunStream error identity", func(t *testing.T) {
		stub.StreamErr = context.Canceled
		_, err := lb.AgentRunStream(ctx, "sess-2", "do it", workspace.AgentRunOptions{})
		require.ErrorIs(t, err, context.Canceled)
		stub.StreamErr = nil
	})

	t.Run("Subscribe", func(t *testing.T) {
		var got any
		lb.Subscribe(func(ev any) { got = ev })
		require.True(t, stub.SubscribeCalled)
		stub.SubscribeSend(wsrpctest.SessionEvent)
		require.Equal(t, wsrpctest.SessionEvent, got)
	})

	t.Run("SubscribeWith", func(t *testing.T) {
		stopped := false
		stub.SubscribeWithStop = func() { stopped = true }
		var got any
		stop := lb.SubscribeWith(func(ev any) { got = ev })
		require.True(t, stub.SubscribeWithCalled)
		ev := pubsub.Event[session.Session]{Type: pubsub.UpdatedEvent, Payload: session.Session{ID: "sess-2"}}
		stub.SubscribeWithSend(ev)
		require.Equal(t, ev, got)
		stop()
		require.True(t, stopped)
	})

	t.Run("Subscribe panics on an unregistered event type", func(t *testing.T) {
		lb.Subscribe(func(any) {})
		require.Panics(t, func() { stub.SubscribeSend(42) })
	})

	t.Run("StartOAuth", func(t *testing.T) {
		stub.OAuthResult = workspace.OAuthStartResult{AuthorizationURL: "https://example.com"}
		got, _, err := lb.StartOAuth(ctx, "github", "", true)
		require.NoError(t, err)
		require.Equal(t, "https://example.com", got.AuthorizationURL)
		require.Equal(t, "github", stub.GotOAuthProviderID)
	})

	t.Run("StartOAuth error identity", func(t *testing.T) {
		stub.OAuthErr = context.Canceled
		_, _, err := lb.StartOAuth(ctx, "github", "", true)
		require.ErrorIs(t, err, context.Canceled)
		stub.OAuthErr = nil
	})

	t.Run("StartOAuth flow Wait", func(t *testing.T) {
		stub.OAuthErr = nil
		innerFlow := &wsrpctest.StubOAuthFlow{Completion: wsrpctest.OAuthCompletionSample}
		stub.OAuthFlow = innerFlow
		_, flow, err := lb.StartOAuth(ctx, "github", "", true)
		require.NoError(t, err)
		require.NotNil(t, flow)
		got, err := flow.Wait(ctx)
		require.NoError(t, err)
		require.Equal(t, wsrpctest.OAuthCompletionSample, got)

		innerFlow.WaitErr = context.Canceled
		_, err = flow.Wait(ctx)
		require.ErrorIs(t, err, context.Canceled)

		flow.Cancel()
		require.True(t, innerFlow.Cancelled)
	})

	t.Run("EnterWorktree", func(t *testing.T) {
		inner := &wsrpctest.StubWorkspace{}
		stub.WorktreeWorkspace = inner
		got, _, err := lb.EnterWorktree(ctx, "feature")
		require.NoError(t, err)
		require.Equal(t, "feature", stub.GotWorktreeName)
		// A wire hop hands back a client stub, never the server's own
		// object -- EnterWorktree wraps it in another Loopback rather
		// than returning inner as-is, so calls made through got also go
		// through the codec.
		require.IsType(t, &wsrpc.Loopback{}, got)
	})

	t.Run("EnterWorktree error identity", func(t *testing.T) {
		stub.WorktreeWorkspace = nil
		stub.WorktreeErr = context.Canceled
		got, _, err := lb.EnterWorktree(ctx, "feature")
		require.Nil(t, got)
		require.ErrorIs(t, err, context.Canceled)
		stub.WorktreeErr = nil
	})

	t.Run("ExitWorktree", func(t *testing.T) {
		inner := &wsrpctest.StubWorkspace{}
		stub.WorktreeWorkspace = inner
		got, _, err := lb.ExitWorktree(ctx)
		require.NoError(t, err)
		require.True(t, stub.ExitWorktreeCalled)
		require.IsType(t, &wsrpc.Loopback{}, got)
	})

	t.Run("AttachThread", func(t *testing.T) {
		inner := &wsrpctest.StubWorkspace{}
		stub.AttachThreadWorkspace = inner
		got, _, err := lb.AttachThread(ctx, "thread-1")
		require.NoError(t, err)
		require.Equal(t, "thread-1", stub.GotAttachThreadID)
		require.IsType(t, &wsrpc.Loopback{}, got)
	})

	t.Run("Shutdown", func(t *testing.T) {
		lb.Shutdown()
		require.True(t, stub.ShutdownCalled)
	})
}
