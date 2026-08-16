package workspace

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/commands"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/stats"
)

// ErrReadOnlyOperation is returned by a read-only workspace when a
// mutating operation is attempted.
type ErrReadOnlyOperation struct {
	Operation string
}

func (e *ErrReadOnlyOperation) Error() string {
	return fmt.Sprintf("read-only workspace: %s is not allowed", e.Operation)
}

// readOnlyWorkspace wraps an underlying Workspace and restricts all
// mutating operations. It is used for completed/interrupted threads
// so the TUI can inspect persisted session data without risking
// accidental writes or tearing down the parent workspace.
type readOnlyWorkspace struct {
	underlying Workspace
	workingDir string
	sessionID  string
}

// readOnlyError returns a typed error for the given operation name.
func (w *readOnlyWorkspace) readOnlyError(op string) error {
	return &ErrReadOnlyOperation{Operation: op}
}

// newReadOnlyWorkspace creates a read-only wrapper over the given workspace.
// workingDir is the thread worktree path (not the parent's workdir).
func newReadOnlyWorkspace(ws Workspace, workingDir, sessionID string) *readOnlyWorkspace {
	return &readOnlyWorkspace{underlying: ws, workingDir: workingDir, sessionID: sessionID}
}

// allowsSession permits the thread's root session and genuine agent-tool
// sessions whose persisted parent chain leads back to that root. Parsing is
// required for every descendant so an arbitrary session that merely claims a
// root parent cannot widen this read-only view.
func (w *readOnlyWorkspace) allowsSession(ctx context.Context, id string) (bool, error) {
	if id == w.sessionID {
		return true, nil
	}
	if _, _, ok := w.underlying.ParseAgentToolSessionID(id); !ok {
		return false, nil
	}

	seen := map[string]struct{}{id: {}}
	for id != w.sessionID {
		sess, err := w.underlying.GetSession(ctx, id)
		if err != nil {
			return false, err
		}
		parentID := sess.ParentSessionID
		if parentID == "" {
			return false, nil
		}
		if parentID == w.sessionID {
			return true, nil
		}
		if _, _, ok := w.underlying.ParseAgentToolSessionID(parentID); !ok {
			return false, nil
		}
		if _, alreadySeen := seen[parentID]; alreadySeen {
			return false, nil
		}
		seen[parentID] = struct{}{}
		id = parentID
	}
	return true, nil
}

func (w *readOnlyWorkspace) scopeError(id string) error {
	return fmt.Errorf("read-only workspace: session %q is outside thread scope", id)
}

// -- Sessions (reads only) --

func (w *readOnlyWorkspace) BackgroundJobCounts() shell.BackgroundJobCounts {
	return w.underlying.BackgroundJobCounts()
}

func (w *readOnlyWorkspace) CreateSession(ctx context.Context, title string) (session.Session, error) {
	return session.Session{}, w.readOnlyError("CreateSession")
}

func (w *readOnlyWorkspace) GetSession(ctx context.Context, sessionID string) (session.Session, error) {
	allowed, err := w.allowsSession(ctx, sessionID)
	if err != nil {
		return session.Session{}, err
	}
	if !allowed {
		return session.Session{}, w.scopeError(sessionID)
	}
	return w.underlying.GetSession(ctx, sessionID)
}

func (w *readOnlyWorkspace) ListSessions(ctx context.Context) ([]session.Session, error) {
	sess, err := w.GetSession(ctx, w.sessionID)
	if err != nil {
		return nil, err
	}
	return []session.Session{sess}, nil
}

func (w *readOnlyWorkspace) SaveSession(ctx context.Context, sess session.Session) (session.Session, error) {
	return session.Session{}, w.readOnlyError("SaveSession")
}

func (w *readOnlyWorkspace) DeleteSession(ctx context.Context, sessionID string) error {
	return w.readOnlyError("DeleteSession")
}

func (w *readOnlyWorkspace) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return w.underlying.CreateAgentToolSessionID(messageID, toolCallID)
}

func (w *readOnlyWorkspace) ParseAgentToolSessionID(sessionID string) (string, string, bool) {
	return w.underlying.ParseAgentToolSessionID(sessionID)
}

func (w *readOnlyWorkspace) SetCurrentSession(ctx context.Context, sessionID string) error {
	return w.SetCurrentSessionGeneration(ctx, sessionID, 0)
}

func (w *readOnlyWorkspace) SetCurrentSessionGeneration(ctx context.Context, sessionID string, _ uint64) error {
	if sessionID == "" {
		return nil
	}
	allowed, err := w.allowsSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if !allowed {
		return w.scopeError(sessionID)
	}
	return nil
}

// -- Messages (reads only) --

func (w *readOnlyWorkspace) ListMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	allowed, err := w.allowsSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, w.scopeError(sessionID)
	}
	return w.underlying.ListMessages(ctx, sessionID)
}

func (w *readOnlyWorkspace) ListUserMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	allowed, err := w.allowsSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, w.scopeError(sessionID)
	}
	return w.underlying.ListUserMessages(ctx, sessionID)
}

func (w *readOnlyWorkspace) ListAllUserMessages(ctx context.Context) ([]message.Message, error) {
	return w.underlying.ListUserMessages(ctx, w.sessionID)
}

func (w *readOnlyWorkspace) ListMessagesBySessionIDs(ctx context.Context, rootSessionID string, generation uint64, sessionIDs []string) (map[string][]message.Message, error) {
	if rootSessionID != w.sessionID {
		return nil, w.scopeError(rootSessionID)
	}
	return w.underlying.ListMessagesBySessionIDs(ctx, w.sessionID, generation, sessionIDs)
}

// -- Agent (query only, no mutations) --

func (w *readOnlyWorkspace) AgentRun(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) error {
	return w.readOnlyError("AgentRun")
}

func (w *readOnlyWorkspace) AgentRunShellCommand(ctx context.Context, sessionID, command string, termWidth int, onProgress func(string), isFirstMessage bool) (proto.ShellCommandResponse, error) {
	return proto.ShellCommandResponse{}, w.readOnlyError("AgentRunShellCommand")
}

func (w *readOnlyWorkspace) AgentCancel(sessionID string) {
	// No-op: cancelling a non-running thread is harmless.
}

func (w *readOnlyWorkspace) AgentIsBusy() bool {
	return w.underlying.AgentIsBusy()
}

func (w *readOnlyWorkspace) AgentIsSessionBusy(sessionID string) bool {
	return w.underlying.AgentIsSessionBusy(sessionID)
}

func (w *readOnlyWorkspace) AgentModel() AgentModel {
	return w.underlying.AgentModel()
}

func (w *readOnlyWorkspace) AgentIsReady() bool {
	return w.underlying.AgentIsReady()
}

func (w *readOnlyWorkspace) AgentReadyErr() error {
	return w.underlying.AgentReadyErr()
}

func (w *readOnlyWorkspace) AgentQueuedPrompts(sessionID string) int {
	return w.underlying.AgentQueuedPrompts(sessionID)
}

func (w *readOnlyWorkspace) AgentQueuedPromptsList(sessionID string) []string {
	return w.underlying.AgentQueuedPromptsList(sessionID)
}

func (w *readOnlyWorkspace) AgentClearQueue(sessionID string) {
	// No-op: clearing an empty queue is harmless.
}

func (w *readOnlyWorkspace) AgentSummarize(ctx context.Context, sessionID string) error {
	return w.readOnlyError("AgentSummarize")
}

func (w *readOnlyWorkspace) UpdateAgentModel(ctx context.Context) error {
	return w.readOnlyError("UpdateAgentModel")
}

func (w *readOnlyWorkspace) InitCoderAgent(ctx context.Context) error {
	return w.readOnlyError("InitCoderAgent")
}

func (w *readOnlyWorkspace) InitCoderAgentNonInteractive(ctx context.Context) error {
	return w.readOnlyError("InitCoderAgentNonInteractive")
}

func (w *readOnlyWorkspace) AgentRunStream(ctx context.Context, sessionID, prompt string) (<-chan AgentRunEvent, error) {
	return nil, w.readOnlyError("AgentRunStream")
}

// -- Permissions (all denied) --

func (w *readOnlyWorkspace) PermissionGrant(perm permission.PermissionRequest) bool {
	return false
}

func (w *readOnlyWorkspace) PermissionGrantPersistent(perm permission.PermissionRequest) bool {
	return false
}

func (w *readOnlyWorkspace) PermissionDeny(perm permission.PermissionRequest) bool {
	return false
}

func (w *readOnlyWorkspace) PermissionSkipRequests() bool {
	return w.underlying.PermissionSkipRequests()
}

func (w *readOnlyWorkspace) PermissionSetSkipRequests(skip bool) {
	// No-op: setting skip on a read-only workspace is harmless.
}

// -- Questions (all denied) --

func (w *readOnlyWorkspace) QuestionAnswer(responses []question.Answer) bool {
	return false
}

func (w *readOnlyWorkspace) QuestionCancel() bool {
	return false
}

// -- FileTracker (reads only) --

func (w *readOnlyWorkspace) UncommittedFiles(ctx context.Context) ([]git.FileChange, error) {
	return w.underlying.UncommittedFiles(ctx)
}

func (w *readOnlyWorkspace) FileTrackerRecordRead(ctx context.Context, sessionID, path string) {
	// No-op: recording reads is harmless.
}

func (w *readOnlyWorkspace) FileTrackerLastReadTime(ctx context.Context, sessionID, path string) time.Time {
	allowed, err := w.allowsSession(ctx, sessionID)
	if err != nil || !allowed {
		return time.Time{}
	}
	return w.underlying.FileTrackerLastReadTime(ctx, sessionID, path)
}

func (w *readOnlyWorkspace) FileTrackerListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	allowed, err := w.allowsSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, w.scopeError(sessionID)
	}
	return w.underlying.FileTrackerListReadFiles(ctx, sessionID)
}

// -- History (reads only) --

func (w *readOnlyWorkspace) ListSessionHistory(ctx context.Context, sessionID string) ([]history.File, error) {
	allowed, err := w.allowsSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, w.scopeError(sessionID)
	}
	return w.underlying.ListSessionHistory(ctx, sessionID)
}

// -- LSP (query only) --

func (w *readOnlyWorkspace) LSPStart(ctx context.Context, path string) {
	// No-op: starting LSP on a read-only workspace is harmless.
}

func (w *readOnlyWorkspace) LSPStopAll(ctx context.Context) {
	// No-op: stopping LSP on a read-only workspace is harmless.
}

func (w *readOnlyWorkspace) LSPGetStates() map[string]LSPClientInfo {
	return w.underlying.LSPGetStates()
}

func (w *readOnlyWorkspace) LSPGetDiagnosticCounts(name string) lsp.DiagnosticCounts {
	return w.underlying.LSPGetDiagnosticCounts(name)
}

// -- Config (reads only) --

func (w *readOnlyWorkspace) Config() *config.Config {
	return w.underlying.Config()
}

func (w *readOnlyWorkspace) WorkingDir() string {
	return w.workingDir
}

func (w *readOnlyWorkspace) Resolver() config.VariableResolver {
	return w.underlying.Resolver()
}

func (w *readOnlyWorkspace) UpdatePreferredModel(scope config.Scope, model config.SelectedModel) error {
	return w.readOnlyError("UpdatePreferredModel")
}

func (w *readOnlyWorkspace) OverridePreferredModel(model config.SelectedModel) error {
	return w.readOnlyError("OverridePreferredModel")
}

func (w *readOnlyWorkspace) SetCompactMode(scope config.Scope, enabled bool) error {
	return w.readOnlyError("SetCompactMode")
}

func (w *readOnlyWorkspace) SetProviderAPIKey(scope config.Scope, providerID string, apiKey any) error {
	return w.readOnlyError("SetProviderAPIKey")
}

func (w *readOnlyWorkspace) SetConfigField(scope config.Scope, key string, value any) error {
	return w.readOnlyError("SetConfigField")
}

func (w *readOnlyWorkspace) RemoveConfigField(scope config.Scope, key string) error {
	return w.readOnlyError("RemoveConfigField")
}

func (w *readOnlyWorkspace) ImportCopilot() (*oauth.Token, bool) {
	return nil, false
}

func (w *readOnlyWorkspace) RefreshOAuthToken(ctx context.Context, scope config.Scope, providerID string) error {
	return w.readOnlyError("RefreshOAuthToken")
}

// -- Project lifecycle (reads only) --

func (w *readOnlyWorkspace) ProjectNeedsInitialization() (bool, error) {
	return w.underlying.ProjectNeedsInitialization()
}

func (w *readOnlyWorkspace) MarkProjectInitialized() error {
	return w.readOnlyError("MarkProjectInitialized")
}

func (w *readOnlyWorkspace) InitializePrompt() (string, error) {
	return w.underlying.InitializePrompt()
}

func (w *readOnlyWorkspace) ListSkills(ctx context.Context) ([]skills.CatalogEntry, error) {
	return w.underlying.ListSkills(ctx)
}

func (w *readOnlyWorkspace) ReadSkill(ctx context.Context, skillID string) ([]byte, skills.SkillReadResult, error) {
	return w.underlying.ReadSkill(ctx, skillID)
}

// -- MCP (query only) --

func (w *readOnlyWorkspace) WaitForMCPInit(ctx context.Context) error {
	return w.underlying.WaitForMCPInit(ctx)
}

// Stats is a read, so it passes straight through: a read-only workspace
// refuses mutations, not questions about what has already happened.
func (w *readOnlyWorkspace) Stats(ctx context.Context, req stats.Request) (stats.Snapshot, error) {
	return w.underlying.Stats(ctx, req)
}

func (w *readOnlyWorkspace) MCPGetStates() map[string]MCPClientInfo {
	return w.underlying.MCPGetStates()
}

func (w *readOnlyWorkspace) MCPResources() []MCPResourceInfo {
	return w.underlying.MCPResources()
}

func (w *readOnlyWorkspace) MCPRefreshPrompts(ctx context.Context, name string) {
	// No-op: refreshing prompts on a read-only workspace is harmless.
}

func (w *readOnlyWorkspace) MCPRefreshResources(ctx context.Context, name string) {
	// No-op: refreshing resources on a read-only workspace is harmless.
}

func (w *readOnlyWorkspace) RefreshMCPTools(ctx context.Context, name string) {
	// No-op: refreshing tools on a read-only workspace is harmless.
}

func (w *readOnlyWorkspace) ReadMCPResource(ctx context.Context, name, uri string) ([]MCPResourceContents, error) {
	return w.underlying.ReadMCPResource(ctx, name, uri)
}

func (w *readOnlyWorkspace) ListMCPPrompts(ctx context.Context) ([]commands.MCPPrompt, error) {
	return w.underlying.ListMCPPrompts(ctx)
}

func (w *readOnlyWorkspace) GetMCPPrompt(clientID, promptID string, args map[string]string) (string, error) {
	return w.underlying.GetMCPPrompt(clientID, promptID, args)
}

func (w *readOnlyWorkspace) EnableDockerMCP(ctx context.Context) error {
	return w.readOnlyError("EnableDockerMCP")
}

func (w *readOnlyWorkspace) DisableDockerMCP() error {
	return w.readOnlyError("DisableDockerMCP")
}

func (w *readOnlyWorkspace) MCPAuthenticate(ctx context.Context, name string) error {
	return w.readOnlyError("MCPAuthenticate")
}

func (w *readOnlyWorkspace) MCPPendingAuth() []MCPPendingAuthServer {
	return w.underlying.MCPPendingAuth()
}

func (w *readOnlyWorkspace) MCPAuthURL(name string) string {
	return w.underlying.MCPAuthURL(name)
}

// -- ThreadController (query only) --

func (w *readOnlyWorkspace) SupportsThreads() bool {
	return w.underlying.SupportsThreads()
}

func (w *readOnlyWorkspace) ListThreads(ctx context.Context) ([]proto.Thread, error) {
	return w.underlying.ListThreads(ctx)
}

func (w *readOnlyWorkspace) GetThread(ctx context.Context, id string) (proto.Thread, error) {
	return w.underlying.GetThread(ctx, id)
}

func (w *readOnlyWorkspace) CreateThread(ctx context.Context, req proto.CreateThreadRequest) (proto.Thread, error) {
	return proto.Thread{}, w.readOnlyError("CreateThread")
}

func (w *readOnlyWorkspace) SendThread(ctx context.Context, id, message string) error {
	return w.readOnlyError("SendThread")
}

func (w *readOnlyWorkspace) ActivateThread(ctx context.Context, id string) (proto.Thread, error) {
	return proto.Thread{}, w.readOnlyError("ActivateThread")
}

func (w *readOnlyWorkspace) MergeThread(ctx context.Context, id string) (proto.Thread, error) {
	return proto.Thread{}, w.readOnlyError("MergeThread")
}

func (w *readOnlyWorkspace) CancelThread(ctx context.Context, id, reason string) error {
	return w.readOnlyError("CancelThread")
}

func (w *readOnlyWorkspace) RemoveThread(ctx context.Context, id string, opts proto.RemoveThreadOptions) error {
	return w.readOnlyError("RemoveThread")
}

func (w *readOnlyWorkspace) AttachThread(ctx context.Context, id string) (Workspace, func(), error) {
	return nil, nil, w.readOnlyError("AttachThread")
}

// threadAttachRefuser is implemented by a Workspace whose AttachThread can
// never succeed, so it can say so up front. Declared by the refusing type
// rather than by every capable one on purpose: a workspace implementation
// (or a test stub) that says nothing is assumed capable, which at worst
// costs one failed call, whereas the opposite default would silently strip
// the capability from anything that forgot to opt in.
type threadAttachRefuser interface {
	refusesThreadAttach()
}

func (w *readOnlyWorkspace) refusesThreadAttach() {}

// SupportsThreadAttach reports whether ws.AttachThread can succeed at all.
// It is false only for a read-only view of a thread (the fallback
// AttachThread itself returns when a thread cannot be reactivated), where
// the refusal is a property of the workspace rather than of the call: every
// attempt fails, forever, so a caller that polls wants to know before it
// starts rather than learning it once per attempt.
//
// This is a capability question, not a permission check, and it is an
// optimization rather than a guarantee — a caller that polls still needs to
// handle a failing AttachThread without spinning. Callers that genuinely
// want to attach should just call AttachThread and handle the error.
func SupportsThreadAttach(ws Workspace) bool {
	_, refuses := ws.(threadAttachRefuser)
	return !refuses
}

// -- TaskController (query only) --

func (w *readOnlyWorkspace) SupportsTasks() bool {
	return w.underlying.SupportsTasks()
}

func (w *readOnlyWorkspace) ListTasks(ctx context.Context) ([]proto.Thread, error) {
	return w.underlying.ListTasks(ctx)
}

func (w *readOnlyWorkspace) CancelTask(ctx context.Context, id, reason string) error {
	return w.readOnlyError("CancelTask")
}

// -- EventSubscriber --

func (w *readOnlyWorkspace) Subscribe(program *tea.Program) {
	// No-op: subscribing to a read-only workspace's events is safe
	// but not meaningful for a completed thread.
}

func (w *readOnlyWorkspace) Shutdown() {
	// No-op: shutting down a read-only workspace must NOT affect
	// the parent workspace. It is safe to call multiple times.
}

// Compile-time check that readOnlyWorkspace implements Workspace.
var _ Workspace = (*readOnlyWorkspace)(nil)

// IsReadOnlyError reports whether err is a read-only operation error.
func IsReadOnlyError(err error) bool {
	var roErr *ErrReadOnlyOperation
	return err != nil && errors.As(err, &roErr)
}
