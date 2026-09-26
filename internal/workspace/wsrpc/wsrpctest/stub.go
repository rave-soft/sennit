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
	"sync/atomic"

	"charm.land/catwalk/pkg/catwalk"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/providers/accounts"
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
	// StreamCtxCh, if non-nil, receives the ctx AgentRunStream was actually
	// called with (best effort, non-blocking send) -- letting a test
	// observe whether the server detached it from the stream's own
	// cancellation (CLIENT-SERVER.md, PR 1.2 build step 1.2c) instead of
	// passing stream.Context() straight through.
	StreamCtxCh chan context.Context

	// AgentCancelCalls records every AgentCancel(sessionID) call, in
	// order -- a test asserts on its length and contents to check "called
	// exactly once" / "never called".
	AgentCancelMu    sync.Mutex
	AgentCancelCalls []string
	AgentCancelErr   error

	SubscribeCalled bool
	SubscribeSend   func(any)

	SubscribeWithCalled bool
	// SubscribeWithCalls counts every SubscribeWith call, for a test that
	// needs to prove "no NEW call happened" rather than just "called at
	// least once" (SubscribeWithCalled) -- e.g. a root hub that already
	// started eagerly at NewServer time (CLIENT-SERVER.md, PR 1.4a) has
	// SubscribeWithCalled true before the test does anything, so the
	// interesting assertion is that a child handle's own subscription
	// never bumps this count further. Safe for concurrent reads/writes
	// (grpcws's per-hub state publisher can call SubscribeWith from its
	// own goroutine).
	SubscribeWithCalls atomic.Int32
	SubscribeWithSend  func(any)
	SubscribeWithStop  func()
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

	// The fields below back every class-C getter (wsrpc.MethodClasses)
	// StubWorkspace did not already have a field for, so
	// wsrpc.BuildClientState's mapping test can drive each one to a
	// distinctive value without a nil-embedded-Workspace panic -- and so
	// every other test built on this stub (most of grpcws's) gets a safe,
	// zero-value answer for a getter it never bothered to set, including
	// from a background goroutine (grpcws's per-hub state publisher ticks
	// on its own schedule, independent of whatever the test is doing).
	AgentIsBusyResult            bool
	AgentIsSessionBusyResult     bool
	AgentIsReadyResult           bool
	AgentReadyErrResult          error
	AgentQueuedPromptsListResult []string
	AgentActivityResult          workspace.AgentActivity
	PermissionSkipRequestsResult bool
	ConfigResult                 *workspace.FrontendConfig
	CurrentPlanUsageResult       accounts.Usage
	CurrentPlanUsageOK           bool
	AccountCapabilitiesResult    workspace.AccountCapabilities
	KnownProvidersResult         []catwalk.Provider
	CustomProviderTypesResult    []string
	DockerMCPAvailableResult     bool
	DockerMCPKnownResult         bool
	MCPPendingAuthResult         []workspace.MCPPendingAuthServer
	MCPAuthURLResult             string
	WorktreeStateResult          workspace.WorktreeState
	SupportsThreadsResult        bool
	SupportsTasksResult          bool
	BackgroundJobCountsResult    workspace.BackgroundJobCounts

	// PendingPromptsResult backs PendingPrompts (class U, not C -- see
	// workspace.PendingPromptsReader's doc comment): the Snapshot RPC's
	// own pending-permission/pending-question collection, settable so a
	// test can prove Snapshot surfaces a request that has no subscriber.
	PendingPromptsResult workspace.PendingPrompts
	PendingPromptsErr    error
}

func (s *StubWorkspace) PendingPrompts(context.Context) (workspace.PendingPrompts, error) {
	return s.PendingPromptsResult, s.PendingPromptsErr
}

func (s *StubWorkspace) WorkingDir() string {
	return s.WorkingDirResult
}

func (s *StubWorkspace) AgentIsBusy() bool { return s.AgentIsBusyResult }

func (s *StubWorkspace) AgentIsSessionBusy(string) bool { return s.AgentIsSessionBusyResult }

func (s *StubWorkspace) AgentIsReady() bool { return s.AgentIsReadyResult }

func (s *StubWorkspace) AgentReadyErr() error { return s.AgentReadyErrResult }

func (s *StubWorkspace) AgentQueuedPromptsList(string) []string {
	return s.AgentQueuedPromptsListResult
}

func (s *StubWorkspace) AgentActivity() workspace.AgentActivity {
	return s.AgentActivityResult
}

func (s *StubWorkspace) PermissionSkipRequests() bool { return s.PermissionSkipRequestsResult }

func (s *StubWorkspace) Config() *workspace.FrontendConfig { return s.ConfigResult }

func (s *StubWorkspace) CurrentPlanUsage(string) (accounts.Usage, bool) {
	return s.CurrentPlanUsageResult, s.CurrentPlanUsageOK
}

func (s *StubWorkspace) AccountCapabilities(string) workspace.AccountCapabilities {
	return s.AccountCapabilitiesResult
}

func (s *StubWorkspace) KnownProviders() []catwalk.Provider { return s.KnownProvidersResult }

func (s *StubWorkspace) CustomProviderTypes() []string { return s.CustomProviderTypesResult }

func (s *StubWorkspace) DockerMCPAvailable() (bool, bool) {
	return s.DockerMCPAvailableResult, s.DockerMCPKnownResult
}

func (s *StubWorkspace) MCPPendingAuth() []workspace.MCPPendingAuthServer {
	return s.MCPPendingAuthResult
}

func (s *StubWorkspace) MCPAuthURL(string) string { return s.MCPAuthURLResult }

func (s *StubWorkspace) WorktreeState() workspace.WorktreeState { return s.WorktreeStateResult }

func (s *StubWorkspace) SupportsThreads() bool { return s.SupportsThreadsResult }

func (s *StubWorkspace) SupportsTasks() bool { return s.SupportsTasksResult }

func (s *StubWorkspace) BackgroundJobCounts() workspace.BackgroundJobCounts {
	return s.BackgroundJobCountsResult
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

func (s *StubWorkspace) AgentRunStream(ctx context.Context, sessionID, _ string, opts workspace.AgentRunOptions) (<-chan workspace.AgentRunEvent, error) {
	s.GotStreamSessionID = sessionID
	s.GotStreamOptions = opts
	if s.StreamCtxCh != nil {
		select {
		case s.StreamCtxCh <- ctx:
		default:
		}
	}
	if s.StreamErr != nil {
		return nil, s.StreamErr
	}
	return s.StreamChan, nil
}

// AgentCancel records sessionID and returns AgentCancelErr, letting a test
// assert how many times (and with what argument) the client sent
// AgentCancel -- e.g. exactly once, following the caller's ctx
// cancellation (CLIENT-SERVER.md, PR 1.2 build step 1.2c).
func (s *StubWorkspace) AgentCancel(sessionID string) error {
	s.AgentCancelMu.Lock()
	defer s.AgentCancelMu.Unlock()
	s.AgentCancelCalls = append(s.AgentCancelCalls, sessionID)
	return s.AgentCancelErr
}

func (s *StubWorkspace) Subscribe(send func(any)) {
	s.SubscribeCalled = true
	s.SubscribeSend = send
}

func (s *StubWorkspace) SubscribeWith(send func(any)) func() {
	s.SubscribeWithCalled = true
	s.SubscribeWithCalls.Add(1)
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
// handle, and a grpcws test exercise OAuthWait/OAuthCancel's own contract:
// the ctx flow.Wait was actually called with (WaitCtxCh -- was it
// stream.Context() passed straight through, per CLIENT-SERVER.md PR
// 1.3b-2, rather than detached the way AgentRunStream's turn is), how many
// times Cancel ran (CancelCalls -- must be exactly one, whether that's
// OAuthWait's own drop, an explicit OAuthCancel call, or a lease-expiry
// sweep), and a controllable completion (WaitDone, to hold Wait open until
// a test is ready to let it finish, or ctx ends first).
type StubOAuthFlow struct {
	Completion workspace.OAuthCompletion
	WaitErr    error

	// WaitCtxCh, if non-nil, receives the ctx Wait was actually called
	// with (best effort, non-blocking send) -- see StreamCtxCh's identical
	// pattern above for why a channel rather than a plain field.
	WaitCtxCh chan context.Context

	// WaitDone, if non-nil, blocks Wait from returning until it's closed,
	// or ctx is done, whichever comes first -- letting a test hold a flow
	// "pending" long enough to sever the connection or expire the lease
	// while OAuthWait is still in flight.
	WaitDone chan struct{}

	// Cancelled is kept for existing callers that only care whether
	// Cancel ran at all; CancelCalls is the same count, for a test that
	// needs to assert it ran exactly once.
	CancelMu    sync.Mutex
	Cancelled   bool
	CancelCalls int
}

func (f *StubOAuthFlow) Wait(ctx context.Context) (workspace.OAuthCompletion, error) {
	if f.WaitCtxCh != nil {
		select {
		case f.WaitCtxCh <- ctx:
		default:
		}
	}
	if f.WaitDone != nil {
		select {
		case <-f.WaitDone:
		case <-ctx.Done():
			return workspace.OAuthCompletion{}, ctx.Err()
		}
	}
	if f.WaitErr != nil {
		return workspace.OAuthCompletion{}, f.WaitErr
	}
	return f.Completion, nil
}

func (f *StubOAuthFlow) Cancel() {
	f.CancelMu.Lock()
	defer f.CancelMu.Unlock()
	f.Cancelled = true
	f.CancelCalls++
}

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
