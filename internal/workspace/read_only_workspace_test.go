package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/stats"
	"github.com/stretchr/testify/require"
)

// TestReadOnlyWorkspace_DeniesMutations verifies all mutating operations
// return a typed read-only error.
func TestReadOnlyWorkspace_DeniesMutations(t *testing.T) {
	t.Parallel()
	stub := &stubWorkspace{}
	ro := NewReadOnlyWorkspace(stub, "/tmp/thread-worktree", "sess-1", "", git.UncommittedFiles)

	// Session mutations denied.
	_, err := ro.CreateSession(t.Context(), "title")
	require.True(t, IsReadOnlyError(err))
	err = ro.DeleteSession(t.Context(), "sess-1")
	require.True(t, IsReadOnlyError(err))
	err = ro.RenameSession(t.Context(), "sess-1", "new title")
	require.True(t, IsReadOnlyError(err))

	// Agent mutations denied.
	err = ro.AgentRun(t.Context(), "sess-1", "hello")
	require.True(t, IsReadOnlyError(err))
	_, err = ro.AgentRunShellCommand(t.Context(), "sess-1", "echo hi", 80, nil, false)
	require.True(t, IsReadOnlyError(err))
	err = ro.AgentSummarize(t.Context(), "sess-1")
	require.True(t, IsReadOnlyError(err))
	_, err = ro.AgentRunStream(t.Context(), "sess-1", "hello", AgentRunOptions{})
	require.True(t, IsReadOnlyError(err))
	err = ro.InitCoderAgent(t.Context())
	require.True(t, IsReadOnlyError(err))
	err = ro.InitCoderAgentNonInteractive(t.Context())
	require.True(t, IsReadOnlyError(err))
	err = ro.UpdateAgentModel(t.Context())
	require.True(t, IsReadOnlyError(err))

	// Importing credentials is a mutation and must not reach the underlying workspace.
	_, imported := ro.ImportCopilot()
	require.False(t, imported)
	require.Zero(t, stub.importCopilotCalls)

	// Config mutations denied.
	err = ro.UpdatePreferredModel(config.ScopeWorkspace, config.SelectedModel{})
	require.True(t, IsReadOnlyError(err))
	err = ro.OverridePreferredModel(config.SelectedModel{})
	require.True(t, IsReadOnlyError(err))
	err = ro.SetCompactMode(config.ScopeWorkspace, true)
	require.True(t, IsReadOnlyError(err))
	err = ro.SetProviderAPIKey(config.ScopeWorkspace, "provider", "key")
	require.True(t, IsReadOnlyError(err))
	err = ro.SetConfigField(config.ScopeWorkspace, "key", "val")
	require.True(t, IsReadOnlyError(err))
	err = ro.RemoveConfigField(config.ScopeWorkspace, "key")
	require.True(t, IsReadOnlyError(err))
	err = ro.RefreshOAuthToken(t.Context(), config.ScopeWorkspace, "provider")
	require.True(t, IsReadOnlyError(err))

	// MCP mutations denied.
	err = ro.EnableDockerMCP(t.Context())
	require.True(t, IsReadOnlyError(err))
	err = ro.DisableDockerMCP()
	require.True(t, IsReadOnlyError(err))
	err = ro.MCPAuthenticate(t.Context(), "name")
	require.True(t, IsReadOnlyError(err))

	// Thread mutations denied.
	_, err = ro.CreateThread(t.Context(), proto.CreateThreadRequest{Name: "x"})
	require.True(t, IsReadOnlyError(err))
	_, err = ro.MergeThread(t.Context(), "id")
	require.True(t, IsReadOnlyError(err))
	err = ro.RemoveThread(t.Context(), "id", proto.RemoveThreadOptions{})
	require.True(t, IsReadOnlyError(err))
	_, _, err = ro.AttachThread(t.Context(), "id")
	require.True(t, IsReadOnlyError(err))

	// Project lifecycle write denied.
	err = ro.MarkProjectInitialized()
	require.True(t, IsReadOnlyError(err))

	// Question resolution denied.
	require.False(t, ro.QuestionAnswer("", nil))
	require.False(t, ro.QuestionCancel())

	// Permission mutations denied.
	require.False(t, ro.PermissionGrant(permission.PermissionRequest{}))
	require.False(t, ro.PermissionGrantPersistent(permission.PermissionRequest{}))
	require.False(t, ro.PermissionDeny(permission.PermissionRequest{}))
}

// TestReadOnlyWorkspace_AllowsReads verifies read-only operations delegate
// to the underlying workspace.
func TestReadOnlyWorkspace_AllowsReads(t *testing.T) {
	t.Parallel()
	stub := &stubWorkspace{}
	ro := NewReadOnlyWorkspace(stub, "/tmp/thread-worktree", "sess-1", "", git.UncommittedFiles)

	// Session reads pass through.
	sess, err := ro.GetSession(t.Context(), "sess-1")
	require.NoError(t, err)
	require.Equal(t, "sess-1", sess.ID)

	sessions, err := ro.ListSessions(t.Context())
	require.NoError(t, err)
	require.Len(t, sessions, 1)

	// Messages pass through.
	msgs, err := ro.ListMessages(t.Context(), "sess-1")
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	userMsgs, err := ro.ListUserMessages(t.Context(), "sess-1")
	require.NoError(t, err)
	require.Len(t, userMsgs, 1)

	allUserMsgs, err := ro.ListAllUserMessages(t.Context())
	require.NoError(t, err)
	require.Len(t, allUserMsgs, 1)

	// WorkingDir is the thread worktree.
	require.Equal(t, "/tmp/thread-worktree", ro.WorkingDir())

	_, err = ro.GetSession(t.Context(), "other-session")
	require.Error(t, err)
	_, err = ro.ListMessages(t.Context(), "other-session")
	require.Error(t, err)
	_, err = ro.ListUserMessages(t.Context(), "other-session")
	require.Error(t, err)
	_, err = ro.FileTrackerListReadFiles(t.Context(), "other-session")
	require.Error(t, err)
	_, err = ro.ListSessionHistory(t.Context(), "other-session")
	require.Error(t, err)
	_, err = ro.PrepareSessionChanges(t.Context(), "other-session")
	require.Error(t, err)

	// Config reads pass through.
	cfg := ro.Config()
	require.NotNil(t, cfg)

	// LSP queries pass through.
	states := ro.LSPGetStates()
	require.IsType(t, map[string]LSPClientInfo{}, states)
	counts := ro.LSPGetDiagnosticCounts("test")
	require.NotNil(t, counts)

	// Agent query state passes through.
	require.False(t, ro.AgentIsBusy())
	require.False(t, ro.AgentIsSessionBusy("sess-1"))
	model := ro.AgentModel()
	require.Equal(t, AgentModel{}, model)
	require.False(t, ro.AgentIsReady())
	require.Error(t, ro.AgentReadyErr())
	require.Nil(t, ro.AgentQueuedPromptsList("sess-1"))

	// File reads pass through.
	require.True(t, ro.FileTrackerLastReadTime(t.Context(), "sess-1", "/foo").IsZero())
	files, err := ro.FileTrackerListReadFiles(t.Context(), "sess-1")
	require.NoError(t, err)
	require.Empty(t, files)

	// History passes through.
	historyFiles, err := ro.ListSessionHistory(t.Context(), "sess-1")
	require.NoError(t, err)
	require.Empty(t, historyFiles)
	// PrepareSessionChanges is computed from this wrapper's own
	// ListSessionHistory and UncommittedFiles, not delegated to the
	// embedded (parent) workspace's PrepareSessionChanges — see
	// TestReadOnlyWorkspace_PrepareSessionChangesScopedToThreadWorktree for
	// the case that actually has files to aggregate.
	changes, err := ro.PrepareSessionChanges(t.Context(), "sess-1")
	require.NoError(t, err)
	require.Empty(t, changes)
	require.NotContains(t, stub.calls, "PrepareSessionChanges")

	// Project lifecycle reads pass through.
	needs, err := ro.ProjectNeedsInitialization()
	require.NoError(t, err)
	require.False(t, needs)

	// MCP query state passes through.
	require.NoError(t, ro.WaitForMCPInit(t.Context()))
	require.IsType(t, map[string]MCPClientInfo{}, ro.MCPGetStates())
	require.IsType(t, []MCPResourceInfo{}, ro.MCPResources())
	resources, err := ro.ReadMCPResource(t.Context(), "name", "uri")
	require.NoError(t, err)
	require.Empty(t, resources)
	_, _ = ro.ListMCPPrompts(t.Context())
	_, _ = ro.GetMCPPrompt("client", "prompt", map[string]string{})
	require.Nil(t, ro.MCPPendingAuth())
	require.Empty(t, ro.MCPAuthURL("name"))

	// Thread query state passes through.
	require.False(t, ro.SupportsThreads())
	threads, err := ro.ListThreads(t.Context())
	require.NoError(t, err)
	require.Empty(t, threads)
	_, err = ro.GetThread(t.Context(), "id")
	require.NoError(t, err)

	// SetCurrentSession only updates local UI state.
	require.NoError(t, ro.SetCurrentSession(t.Context(), "sess-1"))
	require.Error(t, ro.SetCurrentSession(t.Context(), "other-session"))

	// PermissionSkipRequests query passes through.
	require.False(t, ro.PermissionSkipRequests())
}

// TestReadOnlyWorkspace_AllowsOnlyRootToolDescendants verifies nested agent
// tool sessions are accessible only when their persisted parent chain reaches
// the thread root. IDs alone, including root-like prefixes, are insufficient.
func TestReadOnlyWorkspace_AllowsOnlyRootToolDescendants(t *testing.T) {
	t.Parallel()

	stub := &stubWorkspace{sessions: map[string]session.Session{
		"root$$child":                {ID: "root$$child", ParentSessionID: "root"},
		"child-message$$nested-tool": {ID: "child-message$$nested-tool", ParentSessionID: "root$$child"},
		"root$$unrelated":            {ID: "root$$unrelated", ParentSessionID: "other-root"},
		"root$$forged":               {ID: "root$$forged", ParentSessionID: "root-prefix"},
	}}
	ro := NewReadOnlyWorkspace(stub, "/tmp/thread-worktree", "root", "", git.UncommittedFiles)

	for _, id := range []string{"root", "root$$child", "child-message$$nested-tool"} {
		_, err := ro.ListMessages(t.Context(), id)
		require.NoError(t, err, id)
	}
	for _, id := range []string{"root$$unrelated", "root$$forged", "root-not-a-tool"} {
		_, err := ro.ListMessages(t.Context(), id)
		require.Error(t, err, id)
	}
}

// TestReadOnlyWorkspace_ShutdownIsNoop verifies Shutdown is safe and idempotent.
func TestReadOnlyWorkspace_ShutdownIsNoop(t *testing.T) {
	t.Parallel()
	stub := &stubWorkspace{}
	ro := NewReadOnlyWorkspace(stub, "/tmp/thread-worktree", "sess-1", "", git.UncommittedFiles)

	require.NotPanics(t, func() { ro.Shutdown() })
	require.NotPanics(t, func() { ro.Shutdown() })
}

// TestReadOnlyError_TypeCheck verifies the error type.
func TestReadOnlyError_TypeCheck(t *testing.T) {
	t.Parallel()
	err := &ErrReadOnlyOperation{Operation: "AgentRun"}
	require.True(t, IsReadOnlyError(err))
	require.False(t, IsReadOnlyError(nil))
	require.False(t, IsReadOnlyError(context.DeadlineExceeded))
	require.False(t, IsReadOnlyError(context.Canceled))
}

// TestReadOnlyWorkspace_NoopMethods verify no-op methods don't panic.
func TestReadOnlyWorkspace_NoopMethods(t *testing.T) {
	t.Parallel()
	stub := &stubWorkspace{}
	ro := NewReadOnlyWorkspace(stub, "/tmp/thread-worktree", "sess-1", "", git.UncommittedFiles)

	require.NotPanics(t, func() { ro.AgentCancel("sess-1") })
	require.NotPanics(t, func() { ro.AgentClearQueue("sess-1") })
	require.NotPanics(t, func() { ro.Subscribe(nil) })
	require.NotPanics(t, func() { ro.LSPStart(t.Context(), "/tmp") })
	require.NotPanics(t, func() { ro.LSPStopAll(t.Context()) })
	require.NotPanics(t, func() { ro.MCPRefreshPrompts(t.Context(), "test") })
	require.NotPanics(t, func() { ro.MCPRefreshResources(t.Context(), "test") })
	require.NotPanics(t, func() { ro.RefreshMCPTools(t.Context(), "test") })
	require.NotPanics(t, func() { ro.FileTrackerRecordRead(t.Context(), "sess-1", "/foo/bar") })
	require.NotPanics(t, func() { ro.PermissionSetSkipRequests(true) })
}

// --- Stub ---

type stubWorkspace struct {
	sessionID          string
	sessionCreateCount int
	sessions           map[string]session.Session
	importCopilotCalls int
	getSessionCalls    int
	batchRoots         []string
	// lastSession, when set, is returned by GetLastSession in place of the
	// fixed "sess-1" default — used to stand in for "the parent's own last
	// session", distinct from any session in the sessions map.
	lastSession *session.Session
	// uncommitted, when set, is returned by UncommittedFiles in place of
	// the default empty slice — used to stand in for "the parent's own
	// working directory diff".
	uncommitted []git.FileChange
	// historyFiles, when set, is returned by ListSessionHistory in place
	// of the default empty slice.
	historyFiles []history.File
	// calls counts invocations of the mutating methods instrumented with
	// track, by name. Used to prove a refused call never reaches the
	// underlying workspace at all (not just that readOnlyWorkspace
	// returns a refusal on top of forwarding it) — see
	// TestReadOnlyWorkspace_RefusesEveryMutatingMethod.
	calls map[string]int
}

func (s *stubWorkspace) track(name string) {
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[name]++
}

// SessionStore
func (s *stubWorkspace) BackgroundJobCounts() BackgroundJobCounts {
	return BackgroundJobCounts{}
}

func (s *stubWorkspace) CreateSession(ctx context.Context, title string) (session.Session, error) {
	s.track("CreateSession")
	s.sessionCreateCount++
	return session.Session{ID: "s" + string(rune(title[0])), Title: title}, nil
}

func (s *stubWorkspace) GetSession(ctx context.Context, sessionID string) (session.Session, error) {
	s.getSessionCalls++
	if sess, ok := s.sessions[sessionID]; ok {
		return sess, nil
	}
	return session.Session{ID: sessionID}, nil
}

func (s *stubWorkspace) ListSessions(ctx context.Context) ([]session.Session, error) {
	return []session.Session{{ID: "sess-1"}}, nil
}

func (s *stubWorkspace) GetLastSession(ctx context.Context) (session.Session, error) {
	if s.lastSession != nil {
		return *s.lastSession, nil
	}
	return session.Session{ID: "sess-1"}, nil
}

func (s *stubWorkspace) RenameSession(ctx context.Context, sessionID string, title string) error {
	s.track("RenameSession")
	return nil
}

func (s *stubWorkspace) DeleteSession(ctx context.Context, sessionID string) error {
	s.track("DeleteSession")
	return nil
}

func (s *stubWorkspace) SetCurrentSession(ctx context.Context, sessionID string) error {
	return s.SetCurrentSessionGeneration(ctx, sessionID, 0)
}

func (s *stubWorkspace) SetCurrentSessionGeneration(_ context.Context, sessionID string, _ uint64) error {
	s.sessionID = sessionID
	return nil
}

func (s *stubWorkspace) SessionDescendantCost(context.Context, string) (float64, error) {
	return 0, nil
}

// Messages
func (s *stubWorkspace) ListMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	return []message.Message{{ID: "msg-1"}}, nil
}

func (s *stubWorkspace) ListUserMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	return []message.Message{{ID: "user-1"}}, nil
}

func (s *stubWorkspace) ListAllUserMessages(ctx context.Context) ([]message.Message, error) {
	return []message.Message{{ID: "all-1"}}, nil
}

// Agent (query only for stub)
func (s *stubWorkspace) AgentRun(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) error {
	s.track("AgentRun")
	return nil
}

func (s *stubWorkspace) AgentRunShellCommand(ctx context.Context, sessionID, command string, termWidth int, onProgress func(string), isFirstMessage bool) (proto.ShellCommandResponse, error) {
	s.track("AgentRunShellCommand")
	return proto.ShellCommandResponse{}, nil
}
func (s *stubWorkspace) AgentCancel(sessionID string)                     { s.track("AgentCancel") }
func (s *stubWorkspace) AgentIsBusy() bool                                { return false }
func (s *stubWorkspace) AgentIsSessionBusy(sessionID string) bool         { return false }
func (s *stubWorkspace) AgentModel() AgentModel                           { return AgentModel{} }
func (s *stubWorkspace) AgentIsReady() bool                               { return false }
func (s *stubWorkspace) AgentReadyErr() error                             { return ErrAgentNotInitialized }
func (s *stubWorkspace) AgentQueuedPromptsList(sessionID string) []string { return nil }
func (s *stubWorkspace) AgentClearQueue(sessionID string)                 { s.track("AgentClearQueue") }

func (s *stubWorkspace) AgentSummarize(ctx context.Context, sessionID string) error {
	s.track("AgentSummarize")
	return nil
}

func (s *stubWorkspace) UpdateAgentModel(ctx context.Context) error {
	s.track("UpdateAgentModel")
	return nil
}

func (s *stubWorkspace) ApplySessionModel(context.Context, string) (bool, error) {
	s.track("ApplySessionModel")
	return false, nil
}

func (s *stubWorkspace) InitCoderAgent(ctx context.Context) error {
	s.track("InitCoderAgent")
	return nil
}

func (s *stubWorkspace) InitCoderAgentNonInteractive(ctx context.Context) error {
	s.track("InitCoderAgentNonInteractive")
	return nil
}

func (s *stubWorkspace) AgentRunStream(ctx context.Context, sessionID, prompt string, opts AgentRunOptions) (<-chan AgentRunEvent, error) {
	s.track("AgentRunStream")
	return nil, nil
}

func (s *stubWorkspace) ResetAgentToolCache() { s.track("ResetAgentToolCache") }

// PermissionResolver
func (s *stubWorkspace) PermissionGrant(perm permission.PermissionRequest) bool {
	s.track("PermissionGrant")
	return false
}

func (s *stubWorkspace) PermissionGrantPersistent(perm permission.PermissionRequest) bool {
	s.track("PermissionGrantPersistent")
	return false
}

func (s *stubWorkspace) PermissionDeny(perm permission.PermissionRequest) bool {
	s.track("PermissionDeny")
	return false
}
func (s *stubWorkspace) PermissionSkipRequests() bool        { return false }
func (s *stubWorkspace) PermissionSetSkipRequests(skip bool) { s.track("PermissionSetSkipRequests") }

// QuestionResponder
func (s *stubWorkspace) QuestionAnswer(batchID string, responses []question.Answer) bool {
	s.track("QuestionAnswer")
	return false
}
func (s *stubWorkspace) QuestionCancel() bool { s.track("QuestionCancel"); return false }

// FileServices
func (s *stubWorkspace) UncommittedFiles(ctx context.Context) ([]git.FileChange, error) {
	return s.uncommitted, nil
}

func (s *stubWorkspace) FileTrackerRecordRead(ctx context.Context, sessionID, path string) {
	s.track("FileTrackerRecordRead")
}

func (s *stubWorkspace) FileTrackerLastReadTime(ctx context.Context, sessionID, path string) time.Time {
	return time.Time{}
}

func (s *stubWorkspace) FileTrackerListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	return nil, nil
}

// History
func (s *stubWorkspace) ListSessionHistory(ctx context.Context, sessionID string) ([]history.File, error) {
	return s.historyFiles, nil
}

func (s *stubWorkspace) PrepareSessionChanges(ctx context.Context, sessionID string) ([]SessionFile, error) {
	s.track("PrepareSessionChanges")
	return []SessionFile{{FirstVersion: history.File{Path: sessionID}}}, nil
}

// LSP
func (s *stubWorkspace) LSPStart(ctx context.Context, path string) { s.track("LSPStart") }
func (s *stubWorkspace) LSPStopAll(ctx context.Context)            { s.track("LSPStopAll") }
func (s *stubWorkspace) LSPGetStates() map[string]LSPClientInfo    { return nil }
func (s *stubWorkspace) LSPGetDiagnosticCounts(name string) proto.LSPDiagnosticCounts {
	return proto.LSPDiagnosticCounts{}
}

// Config
func (s *stubWorkspace) Config() *config.Config            { return &config.Config{} }
func (s *stubWorkspace) WorkingDir() string                { return "/default" }
func (s *stubWorkspace) Resolver() config.VariableResolver { return config.IdentityResolver() }

// Config mutations (for stub, not blocked)
func (s *stubWorkspace) UpdatePreferredModel(scope config.Scope, model config.SelectedModel) error {
	s.track("UpdatePreferredModel")
	return nil
}

func (s *stubWorkspace) OverridePreferredModel(model config.SelectedModel) error {
	s.track("OverridePreferredModel")
	return nil
}

func (s *stubWorkspace) SetCompactMode(scope config.Scope, enabled bool) error {
	s.track("SetCompactMode")
	return nil
}

func (s *stubWorkspace) SetProviderAPIKey(scope config.Scope, providerID string, apiKey any) error {
	s.track("SetProviderAPIKey")
	return nil
}

func (s *stubWorkspace) ConfigureCustomProvider(ctx context.Context, scope config.Scope, params ConfigureCustomProviderParams) ([]catwalk.Model, error) {
	s.track("ConfigureCustomProvider")
	return nil, nil
}

func (s *stubWorkspace) RecordAccount(scope config.Scope, providerID string, cred accounts.LegacyCredential) (accounts.Account, error) {
	s.track("RecordAccount")
	return accounts.Account{}, nil
}

func (s *stubWorkspace) ListAccounts(providerID string) ([]accounts.Account, error) {
	s.track("ListAccounts")
	return nil, nil
}

func (s *stubWorkspace) ActivateAccount(scope config.Scope, providerID, accountID string) error {
	s.track("ActivateAccount")
	return nil
}

func (s *stubWorkspace) UpdateAccount(providerID string, account accounts.Account) error {
	s.track("UpdateAccount")
	return nil
}

func (s *stubWorkspace) RemoveAccount(scope config.Scope, providerID, accountID string) error {
	s.track("RemoveAccount")
	return nil
}

func (s *stubWorkspace) PurgeAccounts(scope config.Scope, providerID string) error {
	s.track("PurgeAccounts")
	return nil
}

func (s *stubWorkspace) SetProviderProxy(providerID, proxy string) error {
	s.track("SetProviderProxy")
	return nil
}

func (s *stubWorkspace) RefreshAccountLimits(ctx context.Context, providerID string) ([]accounts.Account, error) {
	s.track("RefreshAccountLimits")
	return nil, nil
}

func (s *stubWorkspace) VerifyProviderAPIKey(ctx context.Context, providerID, apiKey string) error {
	s.track("VerifyProviderAPIKey")
	return nil
}

func (s *stubWorkspace) StartOAuth(ctx context.Context, providerID, proxyURL string) (OAuthStartResult, OAuthFlow, error) {
	s.track("StartOAuth")
	return OAuthStartResult{}, nil, nil
}

func (s *stubWorkspace) CompleteOAuth(ctx context.Context, providerID, proxyURL string, token *oauth.Token, forceNewAccount bool) (OAuthCompletion, error) {
	s.track("CompleteOAuth")
	return OAuthCompletion{}, nil
}

func (s *stubWorkspace) OAuthConfiguredProxy(providerID string) string {
	s.track("OAuthConfiguredProxy")
	return ""
}

func (s *stubWorkspace) OAuthValidateProxy(providerID, proxyURL string) error {
	s.track("OAuthValidateProxy")
	return nil
}

func (s *stubWorkspace) DockerMCPAvailable() (bool, bool) {
	s.track("DockerMCPAvailable")
	return false, false
}

func (s *stubWorkspace) RefreshDockerMCPAvailability() bool {
	s.track("RefreshDockerMCPAvailability")
	return false
}

func (s *stubWorkspace) ConfigProblems() []config.Problem {
	s.track("ConfigProblems")
	return nil
}

func (s *stubWorkspace) SkillStates() []*skills.SkillState {
	s.track("SkillStates")
	return nil
}

func (s *stubWorkspace) BuiltinSkills() []*skills.Skill {
	s.track("BuiltinSkills")
	return nil
}

func (s *stubWorkspace) DoctorProblems() []config.Problem {
	s.track("DoctorProblems")
	return nil
}

func (s *stubWorkspace) KnownProviders() []catwalk.Provider {
	s.track("KnownProviders")
	return nil
}

func (s *stubWorkspace) CustomProviderTypes() []string {
	s.track("CustomProviderTypes")
	return nil
}

func (s *stubWorkspace) ListCustomCommands(ctx context.Context) ([]CustomCommand, error) {
	s.track("ListCustomCommands")
	return nil, nil
}

func (s *stubWorkspace) CurrentPlanUsage(providerID string) (accounts.Usage, bool) {
	s.track("CurrentPlanUsage")
	return accounts.Usage{}, false
}

func (s *stubWorkspace) AccountCapabilities(providerID string) AccountCapabilities {
	s.track("AccountCapabilities")
	return AccountCapabilities{}
}

func (s *stubWorkspace) SetConfigField(scope config.Scope, key string, value any) error {
	s.track("SetConfigField")
	return nil
}

func (s *stubWorkspace) RemoveConfigField(scope config.Scope, key string) error {
	s.track("RemoveConfigField")
	return nil
}

func (s *stubWorkspace) RefreshOAuthToken(ctx context.Context, scope config.Scope, providerID string) error {
	s.track("RefreshOAuthToken")
	return nil
}

// ProjectLifecycle
func (s *stubWorkspace) ProjectNeedsInitialization() (bool, error) { return false, nil }

func (s *stubWorkspace) MarkProjectInitialized() error     { s.track("MarkProjectInitialized"); return nil }
func (s *stubWorkspace) InitializePrompt() (string, error) { return "", nil }

// Skills
func (s *stubWorkspace) ListSkills(ctx context.Context) ([]skills.CatalogEntry, error) {
	return nil, nil
}

func (s *stubWorkspace) ReadSkill(ctx context.Context, skillID string) ([]byte, skills.SkillReadResult, error) {
	return nil, skills.SkillReadResult{}, nil
}

// MCP
func (s *stubWorkspace) WaitForMCPInit(ctx context.Context) error { return nil }
func (s *stubWorkspace) MCPGetStates() map[string]MCPClientInfo   { return nil }

func (s *stubWorkspace) Stats(context.Context, stats.Request) (stats.Snapshot, error) {
	return stats.Snapshot{}, nil
}
func (s *stubWorkspace) MCPResources() []MCPResourceInfo { return nil }
func (s *stubWorkspace) MCPRefreshPrompts(ctx context.Context, name string) {
	s.track("MCPRefreshPrompts")
}

func (s *stubWorkspace) MCPRefreshResources(ctx context.Context, name string) {
	s.track("MCPRefreshResources")
}

func (s *stubWorkspace) RefreshMCPTools(ctx context.Context, name string) { s.track("RefreshMCPTools") }

func (s *stubWorkspace) ReadMCPResource(ctx context.Context, name, uri string) ([]MCPResourceContents, error) {
	return nil, nil
}

func (s *stubWorkspace) ListMCPPrompts(ctx context.Context) ([]MCPPrompt, error) {
	return nil, nil
}

func (s *stubWorkspace) GetMCPPrompt(clientID, promptID string, args map[string]string) (string, error) {
	return "", nil
}

func (s *stubWorkspace) EnableDockerMCP(ctx context.Context) error {
	s.track("EnableDockerMCP")
	return nil
}
func (s *stubWorkspace) DisableDockerMCP() error { s.track("DisableDockerMCP"); return nil }
func (s *stubWorkspace) MCPAuthenticate(ctx context.Context, name string) error {
	s.track("MCPAuthenticate")
	return nil
}
func (s *stubWorkspace) MCPPendingAuth() []MCPPendingAuthServer { return nil }
func (s *stubWorkspace) MCPAuthURL(name string) string          { return "" }

// ThreadController (query only for stub)
func (s *stubWorkspace) SupportsThreads() bool                                   { return false }
func (s *stubWorkspace) ListThreads(ctx context.Context) ([]proto.Thread, error) { return nil, nil }
func (s *stubWorkspace) GetThread(ctx context.Context, id string) (proto.Thread, error) {
	return proto.Thread{}, nil
}

func (s *stubWorkspace) CreateThread(ctx context.Context, req proto.CreateThreadRequest) (proto.Thread, error) {
	s.track("CreateThread")
	return proto.Thread{}, nil
}

func (s *stubWorkspace) ActivateThread(ctx context.Context, id string) (proto.Thread, error) {
	s.track("ActivateThread")
	return proto.Thread{}, nil
}

func (s *stubWorkspace) MergeThread(ctx context.Context, id string) (proto.Thread, error) {
	s.track("MergeThread")
	return proto.Thread{}, nil
}

func (s *stubWorkspace) CancelThread(ctx context.Context, id, reason string) error {
	s.track("CancelThread")
	return nil
}

func (s *stubWorkspace) RemoveThread(ctx context.Context, id string, opts proto.RemoveThreadOptions) error {
	s.track("RemoveThread")
	return nil
}

func (s *stubWorkspace) AttachThread(ctx context.Context, id string) (Workspace, func(), error) {
	s.track("AttachThread")
	return nil, nil, nil
}

// TaskController (query only for stub)
func (s *stubWorkspace) SupportsTasks() bool                               { return false }
func (s *stubWorkspace) ListTasks(context.Context) ([]proto.Thread, error) { return nil, nil }
func (s *stubWorkspace) CancelTask(context.Context, string, string) error {
	s.track("CancelTask")
	return nil
}

// EventSubscriber
func (s *stubWorkspace) Subscribe(send func(any)) { s.track("Subscribe") }
func (s *stubWorkspace) Shutdown()                { s.track("Shutdown") }

func (s *stubWorkspace) ListMessagesBySessionIDs(_ context.Context, rootSessionID string, _ uint64, sessionIDs []string) (map[string][]message.Message, error) {
	s.batchRoots = append(s.batchRoots, rootSessionID)
	result := make(map[string][]message.Message)
	for _, sid := range sessionIDs {
		result[sid] = []message.Message{{ID: "msg-" + sid}}
	}
	return result, nil
}

// ImportCopilot
func (s *stubWorkspace) ImportCopilot() (*oauth.Token, bool) {
	s.track("ImportCopilot")
	s.importCopilotCalls++
	return nil, false
}

func TestReadOnlyWorkspace_BatchMessages_ChildAndSibling(t *testing.T) {
	t.Parallel()

	stub := &stubWorkspace{
		sessions: map[string]session.Session{
			"root":         {ID: "root"},
			"root$$child":  {ID: "root$$child", ParentSessionID: "root"},
			"root$$sib":    {ID: "root$$sib", ParentSessionID: "root"},
			"other$$child": {ID: "other$$child", ParentSessionID: "other"},
		},
	}
	ro := NewReadOnlyWorkspace(stub, "/tmp/worktree", "root", "", git.UncommittedFiles)

	msgs, err := ro.ListMessagesBySessionIDs(t.Context(), "root", 7, []string{"root", "root$$child", "root$$sib"})
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	require.Contains(t, msgs, "root")
	require.Contains(t, msgs, "root$$child")
	require.Contains(t, msgs, "root$$sib")
	require.Equal(t, 2, stub.getSessionCalls)
	require.Equal(t, []string{"root"}, stub.batchRoots)
	_, err = ro.ListMessagesBySessionIDs(t.Context(), "root", 8, []string{"root$$child", "other$$child"})
	require.Error(t, err)
	require.Equal(t, []string{"root"}, stub.batchRoots)
	_, err = ro.ListMessagesBySessionIDs(t.Context(), "other", 9, []string{"root$$child"})
	require.Error(t, err)
	require.Equal(t, []string{"root"}, stub.batchRoots)
}

// TestSupportsThreadAttach_ReadOnlyRefusesUpFront pairs the capability
// answer with the behaviour it predicts. A read-only workspace is the one
// place AttachThread can never succeed — it is the fallback AttachThread
// itself returns when a thread cannot be reactivated — so a poller (the
// threads dock's activity probe) needs to know before it starts rather
// than discovering it once per attempt, forever.
func TestSupportsThreadAttach_ReadOnlyRefusesUpFront(t *testing.T) {
	t.Parallel()

	stub := &stubWorkspace{}
	require.True(t, SupportsThreadAttach(stub),
		"a workspace that says nothing is assumed capable; the opposite default would silently strip the capability from any implementation that forgot to opt in")

	ro := NewReadOnlyWorkspace(stub, "/tmp/thread-worktree", "sess-1", "", git.UncommittedFiles)
	require.False(t, SupportsThreadAttach(ro))

	_, _, err := ro.AttachThread(t.Context(), "thread-1")
	require.True(t, IsReadOnlyError(err),
		"the capability answer must match what the call actually does")
}

// requireGit skips the test if git is not on PATH.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
}

// initGitRepo creates a scratch git repo in a fresh temp dir, with local
// user.email/user.name config and one commit so a later uncommitted change
// has something to diff against.
//
// The returned path is resolved with filepath.EvalSymlinks so it matches
// what git itself reports: t.TempDir() can hand back a path that runs
// through a symlink (e.g. macOS's /var -> /private/var), while git
// canonicalises its worktree root. Comparing an unresolved test path
// against git's resolved one would fail on those platforms even though
// production behaviour is correct.
func initGitRepo(t *testing.T) string {
	t.Helper()
	requireGit(t)

	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	dir = resolved
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644))
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "initial commit")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, out)
}

// TestReadOnlyWorkspace_GetLastSessionReportsThreadsOwnSession pins the fix
// for GetLastSession: it must report the thread's own root session, not
// whatever the embedded (parent) workspace's GetLastSession answers. Before
// the fix this forwarded straight to the parent and returned parentLast.
func TestReadOnlyWorkspace_GetLastSessionReportsThreadsOwnSession(t *testing.T) {
	t.Parallel()

	parentLast := session.Session{ID: "parent-last-session"}
	stub := &stubWorkspace{
		lastSession: &parentLast,
		sessions: map[string]session.Session{
			"root": {ID: "root"},
		},
	}
	ro := NewReadOnlyWorkspace(stub, "/tmp/thread-worktree", "root", "", git.UncommittedFiles)

	sess, err := ro.GetLastSession(t.Context())
	require.NoError(t, err)
	require.Equal(t, "root", sess.ID,
		"GetLastSession must report the thread's own root session, not the parent's")
}

// TestReadOnlyWorkspace_UncommittedFilesScopedToThreadWorktree pins the fix
// for UncommittedFiles: it must diff the thread's own worktree
// (workingDir), not whatever the embedded (parent) workspace's
// UncommittedFiles answers. Before the fix this forwarded straight to the
// parent and reported the parent's own uncommitted files.
func TestReadOnlyWorkspace_UncommittedFilesScopedToThreadWorktree(t *testing.T) {
	t.Parallel()

	threadDir := initGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(threadDir, "thread-only.txt"), []byte("mine\n"), 0o644))

	parentDir := initGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(parentDir, "parent-only.txt"), []byte("theirs\n"), 0o644))
	parentChanges, err := git.UncommittedFiles(t.Context(), parentDir)
	require.NoError(t, err)
	require.NotEmpty(t, parentChanges)

	// Stand in for a parent AppWorkspace whose own UncommittedFiles diffs
	// its own working directory.
	stub := &stubWorkspace{uncommitted: parentChanges}
	ro := NewReadOnlyWorkspace(stub, threadDir, "root", "", git.UncommittedFiles)

	files, err := ro.UncommittedFiles(t.Context())
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, filepath.Join(threadDir, "thread-only.txt"), files[0].Path)
}

// TestReadOnlyWorkspace_UncommittedFilesUsesInjectedFunc pins the
// NewReadOnlyWorkspace seam itself: UncommittedFiles must call whatever
// function the constructor was given, scoped to workingDir, rather than
// reaching for git.UncommittedFiles on its own. That keeps the actual git
// subprocess launch out of this package's own code — it lives in the
// caller (appws), which is the only production caller and always passes
// git.UncommittedFiles.
func TestReadOnlyWorkspace_UncommittedFilesUsesInjectedFunc(t *testing.T) {
	t.Parallel()

	want := []git.FileChange{{Path: "injected.txt"}}
	var gotDir string
	fake := func(ctx context.Context, dir string) ([]git.FileChange, error) {
		gotDir = dir
		return want, nil
	}

	stub := &stubWorkspace{}
	ro := NewReadOnlyWorkspace(stub, "/tmp/thread-worktree", "sess-1", "", fake)

	files, err := ro.UncommittedFiles(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, files)
	require.Equal(t, "/tmp/thread-worktree", gotDir)
}

// TestReadOnlyWorkspace_PrepareSessionChangesScopedToThreadWorktree pins the
// fix for PrepareSessionChanges: it must compute the uncommitted diff
// against the thread's own worktree using this wrapper's own
// UncommittedFiles, not by delegating straight to the embedded (parent)
// workspace's PrepareSessionChanges — which would diff the parent's
// repository instead.
func TestReadOnlyWorkspace_PrepareSessionChangesScopedToThreadWorktree(t *testing.T) {
	t.Parallel()

	threadDir := initGitRepo(t)
	changedPath := filepath.Join(threadDir, "changed.txt")
	require.NoError(t, os.WriteFile(changedPath, []byte("mine\n"), 0o644))

	stub := &stubWorkspace{
		historyFiles: []history.File{{Path: changedPath, SessionID: "root"}},
	}
	ro := NewReadOnlyWorkspace(stub, threadDir, "root", "", git.UncommittedFiles)

	files, err := ro.PrepareSessionChanges(t.Context(), "root")
	require.NoError(t, err)
	require.Zero(t, stub.calls["PrepareSessionChanges"],
		"must not delegate to the parent's PrepareSessionChanges")
	require.Len(t, files, 1)
	require.Equal(t, changedPath, files[0].FirstVersion.Path)
	require.True(t, files[0].Uncommitted)
}
