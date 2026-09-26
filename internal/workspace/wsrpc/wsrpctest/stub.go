// Package wsrpctest holds a single stub workspace.Workspace shared by
// wsrpc's Loopback tests and grpcws's gRPC conformance tests -- the same
// table exercised through both transports, per CLIENT-SERVER.md's PR 1.1
// acceptance criteria ("conformance test that runs the SAME table as the
// existing Loopback tests"). It is not a _test.go file itself so both
// external test packages (wsrpc_test, grpcws_test) can import it.
package wsrpctest

import (
	"context"
	"sync"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/workspace"
)

// StubWorkspace records the arguments its stubbed methods were called
// with and returns whatever the test preloaded. The embedded nil
// workspace.Workspace satisfies every method this stub doesn't override;
// calling one of those nil-pointer panics, which is fine as long as no
// test exercises it.
type StubWorkspace struct {
	workspace.Workspace

	GotListMessagesSessionID string
	ListMessagesResult       []message.Message

	GotPermissionGrantArg permission.PermissionRequest
	PermissionGrantOK     bool

	GotGetSessionID string
	GetSessionErr   error
	GetSessionDelay chan struct{} // if non-nil, GetSession blocks on ctx.Done() or this channel

	AgentModelResult workspace.AgentModel

	GotShellSessionID string
	ShellResponse     proto.ShellCommandResponse
	ShellErr          error

	GotStreamSessionID string
	GotStreamOptions   workspace.AgentRunOptions
	StreamChan         <-chan workspace.AgentRunEvent
	StreamErr          error

	SubscribeCalled bool
	SubscribeSend   func(any)

	SubscribeWithCalled bool
	SubscribeWithSend   func(any)
	SubscribeWithStop   func()
	// SubscribeWithReady, if non-nil, is closed once SubscribeWith has
	// recorded its arguments above. A caller across goroutines (e.g.
	// grpcws's tests, where SubscribeWith runs on the gRPC stream
	// handler's own goroutine) reads SubscribeWithSend/Called/Stop
	// unsynchronized otherwise, which -race rightly flags; receiving from
	// this channel first establishes the happens-before edge instead.
	SubscribeWithReady chan struct{}

	GotOAuthProviderID string
	OAuthResult        workspace.OAuthStartResult
	OAuthErr           error
	OAuthFlow          workspace.OAuthFlow

	GotWorktreeName    string
	ExitWorktreeCalled bool
	WorktreeWorkspace  workspace.Workspace
	WorktreeErr        error
	// WorktreeReleased, if non-nil, is closed the first time the release
	// func EnterWorktree/ExitWorktree returned actually runs -- letting a
	// test observe a handle registry's release (CLIENT-SERVER.md, PR 1.3)
	// without racing a plain bool across goroutines.
	WorktreeReleased    chan struct{}
	worktreeReleaseOnce sync.Once

	GotAttachThreadID     string
	AttachThreadWorkspace workspace.Workspace
	// AttachThreadReleased mirrors WorktreeReleased, for AttachThread's own
	// release func.
	AttachThreadReleased    chan struct{}
	attachThreadReleaseOnce sync.Once

	ShutdownCalled bool

	WorkingDirResult string
}

func (s *StubWorkspace) WorkingDir() string {
	return s.WorkingDirResult
}

func (s *StubWorkspace) ListMessages(_ context.Context, sessionID string) ([]message.Message, error) {
	s.GotListMessagesSessionID = sessionID
	return s.ListMessagesResult, nil
}

func (s *StubWorkspace) PermissionGrant(perm permission.PermissionRequest) (bool, error) {
	s.GotPermissionGrantArg = perm
	return s.PermissionGrantOK, nil
}

func (s *StubWorkspace) GetSession(ctx context.Context, sessionID string) (session.Session, error) {
	s.GotGetSessionID = sessionID
	if s.GetSessionDelay != nil {
		select {
		case <-ctx.Done():
			return session.Session{}, ctx.Err()
		case <-s.GetSessionDelay:
		}
	}
	if s.GetSessionErr != nil {
		return session.Session{}, s.GetSessionErr
	}
	return session.Session{ID: sessionID, Title: "loopback test session"}, nil
}

func (s *StubWorkspace) AgentModel() workspace.AgentModel {
	return s.AgentModelResult
}

func (s *StubWorkspace) AgentRunShellCommand(_ context.Context, sessionID, _ string, _ int, _ func(string), _ bool) (proto.ShellCommandResponse, error) {
	s.GotShellSessionID = sessionID
	return s.ShellResponse, s.ShellErr
}

func (s *StubWorkspace) AgentRunStream(_ context.Context, sessionID, _ string, opts workspace.AgentRunOptions) (<-chan workspace.AgentRunEvent, error) {
	s.GotStreamSessionID = sessionID
	s.GotStreamOptions = opts
	if s.StreamErr != nil {
		return nil, s.StreamErr
	}
	return s.StreamChan, nil
}

func (s *StubWorkspace) Subscribe(send func(any)) {
	s.SubscribeCalled = true
	s.SubscribeSend = send
}

func (s *StubWorkspace) SubscribeWith(send func(any)) func() {
	s.SubscribeWithCalled = true
	s.SubscribeWithSend = send
	if s.SubscribeWithReady != nil {
		close(s.SubscribeWithReady)
	}
	return s.SubscribeWithStop
}

func (s *StubWorkspace) StartOAuth(_ context.Context, providerID, _ string, _ bool) (workspace.OAuthStartResult, workspace.OAuthFlow, error) {
	s.GotOAuthProviderID = providerID
	if s.OAuthErr != nil {
		return workspace.OAuthStartResult{}, nil, s.OAuthErr
	}
	return s.OAuthResult, s.OAuthFlow, nil
}

func (s *StubWorkspace) EnterWorktree(_ context.Context, name string) (workspace.Workspace, func(), error) {
	s.GotWorktreeName = name
	if s.WorktreeErr != nil {
		return nil, nil, s.WorktreeErr
	}
	return s.WorktreeWorkspace, s.worktreeRelease, nil
}

func (s *StubWorkspace) ExitWorktree(context.Context) (workspace.Workspace, func(), error) {
	s.ExitWorktreeCalled = true
	return s.WorktreeWorkspace, s.worktreeRelease, nil
}

func (s *StubWorkspace) worktreeRelease() {
	s.worktreeReleaseOnce.Do(func() {
		if s.WorktreeReleased != nil {
			close(s.WorktreeReleased)
		}
	})
}

func (s *StubWorkspace) AttachThread(_ context.Context, id string) (workspace.Workspace, func(), error) {
	s.GotAttachThreadID = id
	return s.AttachThreadWorkspace, s.attachThreadRelease, nil
}

func (s *StubWorkspace) attachThreadRelease() {
	s.attachThreadReleaseOnce.Do(func() {
		if s.AttachThreadReleased != nil {
			close(s.AttachThreadReleased)
		}
	})
}

func (s *StubWorkspace) Shutdown() {
	s.ShutdownCalled = true
}

// StubOAuthFlow is workspace.OAuthFlow's stub implementation, letting a
// StartOAuth test exercise a codec/transport's wrapping of the returned
// handle.
type StubOAuthFlow struct {
	Completion workspace.OAuthCompletion
	WaitErr    error
	Cancelled  bool
}

func (f *StubOAuthFlow) Wait(context.Context) (workspace.OAuthCompletion, error) {
	if f.WaitErr != nil {
		return workspace.OAuthCompletion{}, f.WaitErr
	}
	return f.Completion, nil
}

func (f *StubOAuthFlow) Cancel() { f.Cancelled = true }

// OAuthCompletionSample is a representative workspace.OAuthCompletion value
// for StartOAuth/Wait tests.
var OAuthCompletionSample = workspace.OAuthCompletion{
	Account:       workspace.FrontendAccount{ID: "acc_1", Label: "Sample Account"},
	ModelsFetched: 3,
}

// SessionSample, PermissionRequestSample and MessageSample are
// representative non-trivial values for the conformance table (matching
// what wsrpc's own loopback_test.go used before this package existed).
var (
	MessageSample = []message.Message{{
		ID:        "msg-1",
		Role:      message.Assistant,
		SessionID: "sess-1",
		Parts: []message.ContentPart{
			message.TextContent{Text: "hello there"},
			message.ReasoningContent{Thinking: "let me think", Signature: "sig-1"},
			message.ToolCall{ID: "call-1", Name: "bash", Input: `{"command":"echo hi"}`},
		},
		Model:               "gpt-5",
		Provider:            "openai",
		CreatedAt:           1000,
		UpdatedAt:           2000,
		SummaryBeforeTokens: 500,
	}}

	PermissionRequestSample = permission.PermissionRequest{
		ID:          "perm-1",
		SessionID:   "sess-1",
		ToolCallID:  "call-1",
		ToolName:    proto.BashToolName,
		Description: "run a command",
		Action:      "run",
		Path:        "/tmp",
		Params:      proto.BashPermissionsParams{Command: "echo hi", WorkingDir: "/tmp"},
	}

	AgentModelSample = workspace.AgentModel{
		CatalogCfg: workspace.AgentCatalog{ID: "gpt-5", Name: "GPT-5", CanReason: true, ContextWindow: 128000},
		ModelCfg:   workspace.AgentSelection{Provider: "openai", Model: "gpt-5", Think: true, ReasoningEffort: "high"},
	}
)

// SessionEvent is a representative pubsub.Event[session.Session], used by
// Subscribe/SubscribeWith conformance checks.
var SessionEvent = pubsub.Event[session.Session]{Type: pubsub.CreatedEvent, Payload: session.Session{ID: "sess-1"}}
