// Package wsrpc_test (external): exercises Loopback end to end against a
// stub workspace.Workspace, the way PR 0.7's own acceptance criteria asks
// for -- a handful of representative U and C methods, non-trivial values,
// and one error path that must survive the JSON round trip with its
// sentinel identity intact.
package wsrpc_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
	"github.com/stretchr/testify/require"
)

// stubWorkspace records the arguments its stubbed methods were called
// with and returns whatever the test preloaded. The embedded nil
// workspace.Workspace satisfies every method this test doesn't stub;
// calling one of those would nil-pointer panic, which is fine -- nothing
// here does.
type stubWorkspace struct {
	workspace.Workspace

	gotListMessagesSessionID string
	listMessagesResult       []message.Message

	gotPermissionGrantArg permission.PermissionRequest
	permissionGrantOK     bool

	gotGetSessionID string
	getSessionErr   error

	agentModel workspace.AgentModel
}

func (s *stubWorkspace) ListMessages(_ context.Context, sessionID string) ([]message.Message, error) {
	s.gotListMessagesSessionID = sessionID
	return s.listMessagesResult, nil
}

func (s *stubWorkspace) PermissionGrant(perm permission.PermissionRequest) (bool, error) {
	s.gotPermissionGrantArg = perm
	return s.permissionGrantOK, nil
}

func (s *stubWorkspace) GetSession(_ context.Context, sessionID string) (session.Session, error) {
	s.gotGetSessionID = sessionID
	if s.getSessionErr != nil {
		return session.Session{}, s.getSessionErr
	}
	return session.Session{ID: sessionID, Title: "loopback test session"}, nil
}

func (s *stubWorkspace) AgentModel() workspace.AgentModel {
	return s.agentModel
}

func TestLoopback_ListMessages_RoundTripsPartsAndArgs(t *testing.T) {
	t.Parallel()

	want := []message.Message{{
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
	stub := &stubWorkspace{listMessagesResult: want}
	lb := wsrpc.NewLoopback(stub)

	got, err := lb.ListMessages(context.Background(), "sess-1")
	require.NoError(t, err)
	require.Equal(t, "sess-1", stub.gotListMessagesSessionID)
	require.Equal(t, want, got)
}

func TestLoopback_PermissionGrant_RoundTripsParams(t *testing.T) {
	t.Parallel()

	want := permission.PermissionRequest{
		ID:          "perm-1",
		SessionID:   "sess-1",
		ToolCallID:  "call-1",
		ToolName:    proto.BashToolName,
		Description: "run a command",
		Action:      "run",
		Path:        "/tmp",
		Params:      proto.BashPermissionsParams{Command: "echo hi", WorkingDir: "/tmp"},
	}
	stub := &stubWorkspace{permissionGrantOK: true}
	lb := wsrpc.NewLoopback(stub)

	ok, err := lb.PermissionGrant(want)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, stub.gotPermissionGrantArg)
}

func TestLoopback_GetSession_RoundTripsSession(t *testing.T) {
	t.Parallel()

	stub := &stubWorkspace{}
	lb := wsrpc.NewLoopback(stub)

	got, err := lb.GetSession(context.Background(), "sess-42")
	require.NoError(t, err)
	require.Equal(t, "sess-42", stub.gotGetSessionID)
	require.Equal(t, session.Session{ID: "sess-42", Title: "loopback test session"}, got)
}

// TestLoopback_GetSession_ErrorSurvivesRoundTrip checks that an error
// carrying a sentinel code (session.ErrNotFound, wrapped rather than
// returned bare -- the ordinary shape a real implementation would produce)
// still satisfies errors.Is on the decoded side, per workspace.EncodeError/
// DecodeError's contract.
func TestLoopback_GetSession_ErrorSurvivesRoundTrip(t *testing.T) {
	t.Parallel()

	stub := &stubWorkspace{getSessionErr: fmt.Errorf("looking up session: %w", session.ErrNotFound)}
	lb := wsrpc.NewLoopback(stub)

	_, err := lb.GetSession(context.Background(), "missing")
	require.Error(t, err)
	require.True(t, errors.Is(err, session.ErrNotFound), "decoded error should errors.Is session.ErrNotFound, got: %v", err)
}

func TestLoopback_AgentModel_CachedGetterRoundTrips(t *testing.T) {
	t.Parallel()

	want := workspace.AgentModel{
		CatalogCfg: workspace.AgentCatalog{ID: "gpt-5", Name: "GPT-5", CanReason: true, ContextWindow: 128000},
		ModelCfg:   workspace.AgentSelection{Provider: "openai", Model: "gpt-5", Think: true, ReasoningEffort: "high"},
	}
	stub := &stubWorkspace{agentModel: want}
	lb := wsrpc.NewLoopback(stub)

	got := lb.AgentModel()
	require.Equal(t, want, got)
}
