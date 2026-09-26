package wsrpc_test

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
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
	stub := &passthroughStub{}
	lb := wsrpc.NewLoopback(stub)
	ctx := context.Background()

	t.Run("AgentRunShellCommand", func(t *testing.T) {
		stub.shellResponse = proto.ShellCommandResponse{Output: "hi"}
		got, err := lb.AgentRunShellCommand(ctx, "sess-1", "echo hi", 80, nil, true)
		require.NoError(t, err)
		require.Equal(t, "sess-1", stub.gotShellSessionID)
		require.Equal(t, proto.ShellCommandResponse{Output: "hi"}, got)
	})

	t.Run("AgentRunShellCommand error identity", func(t *testing.T) {
		stub.shellErr = context.Canceled
		_, err := lb.AgentRunShellCommand(ctx, "sess-1", "echo hi", 80, nil, true)
		require.ErrorIs(t, err, context.Canceled)
		stub.shellErr = nil
	})

	t.Run("AgentRunStream", func(t *testing.T) {
		ch := make(chan workspace.AgentRunEvent, 1)
		stub.streamChan = ch
		got, err := lb.AgentRunStream(ctx, "sess-2", "do it", workspace.AgentRunOptions{AutoApprovePermissions: true})
		require.NoError(t, err)
		require.Equal(t, "sess-2", stub.gotStreamSessionID)
		require.True(t, stub.gotStreamOptions.AutoApprovePermissions)

		ch <- workspace.AgentRunEvent{TextDelta: "hi", Status: "thinking"}
		close(ch)
		require.Equal(t, workspace.AgentRunEvent{TextDelta: "hi", Status: "thinking"}, <-got)
		_, open := <-got
		require.False(t, open, "loopback's forwarding channel must close once inner does")
	})

	t.Run("AgentRunStream error identity", func(t *testing.T) {
		stub.streamErr = context.Canceled
		_, err := lb.AgentRunStream(ctx, "sess-2", "do it", workspace.AgentRunOptions{})
		require.ErrorIs(t, err, context.Canceled)
		stub.streamErr = nil
	})

	t.Run("Subscribe", func(t *testing.T) {
		var got any
		lb.Subscribe(func(ev any) { got = ev })
		require.True(t, stub.subscribeCalled)
		stub.subscribeSend(pubsub.Event[session.Session]{Type: pubsub.CreatedEvent, Payload: session.Session{ID: "sess-1"}})
		require.Equal(t, pubsub.Event[session.Session]{Type: pubsub.CreatedEvent, Payload: session.Session{ID: "sess-1"}}, got)
	})

	t.Run("SubscribeWith", func(t *testing.T) {
		stopped := false
		stub.subscribeWithStop = func() { stopped = true }
		var got any
		stop := lb.SubscribeWith(func(ev any) { got = ev })
		require.True(t, stub.subscribeWithCalled)
		stub.subscribeWithSend(pubsub.Event[session.Session]{Type: pubsub.UpdatedEvent, Payload: session.Session{ID: "sess-2"}})
		require.Equal(t, pubsub.Event[session.Session]{Type: pubsub.UpdatedEvent, Payload: session.Session{ID: "sess-2"}}, got)
		stop()
		require.True(t, stopped)
	})

	t.Run("Subscribe panics on an unregistered event type", func(t *testing.T) {
		lb.Subscribe(func(any) {})
		require.Panics(t, func() { stub.subscribeSend(42) })
	})

	t.Run("StartOAuth", func(t *testing.T) {
		stub.oauthResult = workspace.OAuthStartResult{AuthorizationURL: "https://example.com"}
		got, _, err := lb.StartOAuth(ctx, "github", "", true)
		require.NoError(t, err)
		require.Equal(t, "https://example.com", got.AuthorizationURL)
		require.Equal(t, "github", stub.gotOAuthProviderID)
	})

	t.Run("StartOAuth error identity", func(t *testing.T) {
		stub.oauthErr = context.Canceled
		_, _, err := lb.StartOAuth(ctx, "github", "", true)
		require.ErrorIs(t, err, context.Canceled)
		stub.oauthErr = nil
	})

	t.Run("StartOAuth flow Wait", func(t *testing.T) {
		stub.oauthErr = nil
		innerFlow := &stubOAuthFlow{token: &oauthTokenSample}
		stub.oauthFlow = innerFlow
		_, flow, err := lb.StartOAuth(ctx, "github", "", true)
		require.NoError(t, err)
		require.NotNil(t, flow)
		got, err := flow.Wait(ctx)
		require.NoError(t, err)
		require.Equal(t, oauthTokenSample, *got)

		innerFlow.waitErr = context.Canceled
		_, err = flow.Wait(ctx)
		require.ErrorIs(t, err, context.Canceled)

		flow.Cancel()
		require.True(t, innerFlow.cancelled)
	})

	t.Run("EnterWorktree", func(t *testing.T) {
		inner := &passthroughStub{}
		stub.worktreeWorkspace = inner
		got, _, err := lb.EnterWorktree(ctx, "feature")
		require.NoError(t, err)
		require.Equal(t, "feature", stub.gotWorktreeName)
		// A wire hop hands back a client stub, never the server's own
		// object -- EnterWorktree wraps it in another Loopback rather
		// than returning inner as-is, so calls made through got also go
		// through the codec.
		require.IsType(t, &wsrpc.Loopback{}, got)
	})

	t.Run("EnterWorktree error identity", func(t *testing.T) {
		stub.worktreeWorkspace = nil
		stub.worktreeErr = context.Canceled
		got, _, err := lb.EnterWorktree(ctx, "feature")
		require.Nil(t, got)
		require.ErrorIs(t, err, context.Canceled)
		stub.worktreeErr = nil
	})

	t.Run("ExitWorktree", func(t *testing.T) {
		inner := &passthroughStub{}
		stub.worktreeWorkspace = inner
		got, _, err := lb.ExitWorktree(ctx)
		require.NoError(t, err)
		require.True(t, stub.exitWorktreeCalled)
		require.IsType(t, &wsrpc.Loopback{}, got)
	})

	t.Run("AttachThread", func(t *testing.T) {
		inner := &passthroughStub{}
		stub.attachThreadWorkspace = inner
		got, _, err := lb.AttachThread(ctx, "thread-1")
		require.NoError(t, err)
		require.Equal(t, "thread-1", stub.gotAttachThreadID)
		require.IsType(t, &wsrpc.Loopback{}, got)
	})

	t.Run("Shutdown", func(t *testing.T) {
		lb.Shutdown()
		require.True(t, stub.shutdownCalled)
	})
}

var oauthTokenSample = oauth.Token{AccessToken: "access-token", RefreshToken: "refresh-token", ExpiresIn: 3600}

// passthroughStub implements just the S/H/X methods loopback_manual.go
// covers; every other Workspace method comes from the embedded nil
// interface and would nil-pointer panic if called, which this test never
// does.
type passthroughStub struct {
	workspace.Workspace

	gotShellSessionID string
	shellResponse     proto.ShellCommandResponse
	shellErr          error

	gotStreamSessionID string
	gotStreamOptions   workspace.AgentRunOptions
	streamChan         <-chan workspace.AgentRunEvent
	streamErr          error

	subscribeCalled bool
	subscribeSend   func(any)

	subscribeWithCalled bool
	subscribeWithSend   func(any)
	subscribeWithStop   func()

	gotOAuthProviderID string
	oauthResult        workspace.OAuthStartResult
	oauthErr           error
	oauthFlow          workspace.OAuthFlow

	gotWorktreeName    string
	exitWorktreeCalled bool
	worktreeWorkspace  workspace.Workspace
	worktreeErr        error

	gotAttachThreadID     string
	attachThreadWorkspace workspace.Workspace

	shutdownCalled bool
}

func (s *passthroughStub) AgentRunShellCommand(_ context.Context, sessionID, _ string, _ int, _ func(string), _ bool) (proto.ShellCommandResponse, error) {
	s.gotShellSessionID = sessionID
	return s.shellResponse, s.shellErr
}

func (s *passthroughStub) AgentRunStream(_ context.Context, sessionID, _ string, opts workspace.AgentRunOptions) (<-chan workspace.AgentRunEvent, error) {
	s.gotStreamSessionID = sessionID
	s.gotStreamOptions = opts
	if s.streamErr != nil {
		return nil, s.streamErr
	}
	return s.streamChan, nil
}

func (s *passthroughStub) Subscribe(send func(any)) {
	s.subscribeCalled = true
	s.subscribeSend = send
}

func (s *passthroughStub) SubscribeWith(send func(any)) func() {
	s.subscribeWithCalled = true
	s.subscribeWithSend = send
	return s.subscribeWithStop
}

func (s *passthroughStub) StartOAuth(_ context.Context, providerID, _ string, _ bool) (workspace.OAuthStartResult, workspace.OAuthFlow, error) {
	s.gotOAuthProviderID = providerID
	if s.oauthErr != nil {
		return workspace.OAuthStartResult{}, nil, s.oauthErr
	}
	return s.oauthResult, s.oauthFlow, nil
}

func (s *passthroughStub) EnterWorktree(_ context.Context, name string) (workspace.Workspace, func(), error) {
	s.gotWorktreeName = name
	if s.worktreeErr != nil {
		return nil, nil, s.worktreeErr
	}
	return s.worktreeWorkspace, func() {}, nil
}

func (s *passthroughStub) ExitWorktree(context.Context) (workspace.Workspace, func(), error) {
	s.exitWorktreeCalled = true
	return s.worktreeWorkspace, func() {}, nil
}

func (s *passthroughStub) AttachThread(_ context.Context, id string) (workspace.Workspace, func(), error) {
	s.gotAttachThreadID = id
	return s.attachThreadWorkspace, func() {}, nil
}

func (s *passthroughStub) Shutdown() {
	s.shutdownCalled = true
}

// stubOAuthFlow is workspace.OAuthFlow's stub implementation, letting the
// StartOAuth test exercise Loopback's codecOAuthFlow wrapper.
type stubOAuthFlow struct {
	token     *oauth.Token
	waitErr   error
	cancelled bool
}

func (f *stubOAuthFlow) Wait(context.Context) (*oauth.Token, error) {
	if f.waitErr != nil {
		return nil, f.waitErr
	}
	return f.token, nil
}

func (f *stubOAuthFlow) Cancel() { f.cancelled = true }
