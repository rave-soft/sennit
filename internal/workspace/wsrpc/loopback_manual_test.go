package wsrpc_test

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
	"github.com/stretchr/testify/require"
)

// TestLoopback_ManualPassthroughMethods exercises loopback_manual.go's
// hand-written S/H/X methods -- plain delegation to inner, with no codec
// involved (that comes in a later PR). Without a test calling each of
// them, they are unreachable from both main and the tests and
// scripts/check_deadcode.sh flags them; this is real coverage instead of
// an allowlist entry, since a broken delegation here is exactly the kind
// of bug that check exists to catch.
// Subtests share one stub instance and run sequentially (no t.Parallel()):
// each checks the args the previous subtest's call left behind, so
// running them concurrently would race on the stub's fields.
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

	t.Run("AgentRunStream", func(t *testing.T) {
		ch := make(chan workspace.AgentRunEvent)
		stub.streamChan = ch
		got, err := lb.AgentRunStream(ctx, "sess-2", "do it", workspace.AgentRunOptions{AutoApprovePermissions: true})
		require.NoError(t, err)
		require.Equal(t, (<-chan workspace.AgentRunEvent)(ch), got)
		require.Equal(t, "sess-2", stub.gotStreamSessionID)
	})

	t.Run("Subscribe", func(t *testing.T) {
		var called bool
		lb.Subscribe(func(any) { called = true })
		require.True(t, stub.subscribeCalled)
		stub.subscribeSend(nil)
		require.True(t, called)
	})

	t.Run("SubscribeWith", func(t *testing.T) {
		stopped := false
		stub.subscribeWithStop = func() { stopped = true }
		stop := lb.SubscribeWith(func(any) {})
		require.True(t, stub.subscribeWithCalled)
		stop()
		require.True(t, stopped)
	})

	t.Run("StartOAuth", func(t *testing.T) {
		stub.oauthResult = workspace.OAuthStartResult{AuthorizationURL: "https://example.com"}
		got, _, err := lb.StartOAuth(ctx, "github", "", true)
		require.NoError(t, err)
		require.Equal(t, "https://example.com", got.AuthorizationURL)
		require.Equal(t, "github", stub.gotOAuthProviderID)
	})

	t.Run("EnterWorktree", func(t *testing.T) {
		inner := &passthroughStub{}
		stub.worktreeWorkspace = inner
		got, _, err := lb.EnterWorktree(ctx, "feature")
		require.NoError(t, err)
		require.Same(t, inner, got)
		require.Equal(t, "feature", stub.gotWorktreeName)
	})

	t.Run("ExitWorktree", func(t *testing.T) {
		inner := &passthroughStub{}
		stub.worktreeWorkspace = inner
		got, _, err := lb.ExitWorktree(ctx)
		require.NoError(t, err)
		require.Same(t, inner, got)
		require.True(t, stub.exitWorktreeCalled)
	})

	t.Run("AttachThread", func(t *testing.T) {
		inner := &passthroughStub{}
		stub.attachThreadWorkspace = inner
		got, _, err := lb.AttachThread(ctx, "thread-1")
		require.NoError(t, err)
		require.Same(t, inner, got)
		require.Equal(t, "thread-1", stub.gotAttachThreadID)
	})

	t.Run("Shutdown", func(t *testing.T) {
		lb.Shutdown()
		require.True(t, stub.shutdownCalled)
	})
}

// passthroughStub implements just the S/H/X methods loopback_manual.go
// covers; every other Workspace method comes from the embedded nil
// interface and would nil-pointer panic if called, which this test never
// does.
type passthroughStub struct {
	workspace.Workspace

	gotShellSessionID string
	shellResponse     proto.ShellCommandResponse

	gotStreamSessionID string
	streamChan         <-chan workspace.AgentRunEvent

	subscribeCalled bool
	subscribeSend   func(any)

	subscribeWithCalled bool
	subscribeWithStop   func()

	gotOAuthProviderID string
	oauthResult        workspace.OAuthStartResult

	gotWorktreeName    string
	exitWorktreeCalled bool
	worktreeWorkspace  workspace.Workspace

	gotAttachThreadID     string
	attachThreadWorkspace workspace.Workspace

	shutdownCalled bool
}

func (s *passthroughStub) AgentRunShellCommand(_ context.Context, sessionID, _ string, _ int, _ func(string), _ bool) (proto.ShellCommandResponse, error) {
	s.gotShellSessionID = sessionID
	return s.shellResponse, nil
}

func (s *passthroughStub) AgentRunStream(_ context.Context, sessionID, _ string, _ workspace.AgentRunOptions) (<-chan workspace.AgentRunEvent, error) {
	s.gotStreamSessionID = sessionID
	return s.streamChan, nil
}

func (s *passthroughStub) Subscribe(send func(any)) {
	s.subscribeCalled = true
	s.subscribeSend = send
}

func (s *passthroughStub) SubscribeWith(func(any)) func() {
	s.subscribeWithCalled = true
	return s.subscribeWithStop
}

func (s *passthroughStub) StartOAuth(_ context.Context, providerID, _ string, _ bool) (workspace.OAuthStartResult, workspace.OAuthFlow, error) {
	s.gotOAuthProviderID = providerID
	return s.oauthResult, nil, nil
}

func (s *passthroughStub) EnterWorktree(_ context.Context, name string) (workspace.Workspace, func(), error) {
	s.gotWorktreeName = name
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
