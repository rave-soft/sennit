package workspace

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/session"
)

// ErrReadOnlyOperation is returned by a read-only workspace when a
// mutating operation is attempted.
type ErrReadOnlyOperation struct {
	Operation string
	// Reason says why this workspace is read-only rather than writable,
	// in the words of whatever refused to make it writable. A thread is
	// opened read-only only as a fallback (see AppWorkspace.AttachThread),
	// and without carrying the reason this far the refusal the user
	// eventually runs into names the symptom and nothing else - the thread
	// looks ordinary until they type into it and are told a bare "not
	// allowed". Empty when the caller had nothing to say.
	Reason string
}

func (e *ErrReadOnlyOperation) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("read-only workspace: %s is not allowed", e.Operation)
	}
	return fmt.Sprintf("read-only workspace: %s is not allowed: the thread could not be reactivated: %s", e.Operation, e.Reason)
}

// readOnlyWorkspace wraps an underlying Workspace and restricts all
// mutating operations. It is used for completed/interrupted threads
// so the TUI can inspect persisted session data without risking
// accidental writes or tearing down the parent workspace.
//
// It embeds Workspace rather than hand-writing a stub for all ~93 methods:
// everything not overridden below is the embedded workspace's own
// behavior, unchanged. That inverts the old failure mode. Before, a
// forgotten stub was a compile error the moment Workspace grew a method.
// Now, a forgotten override compiles fine and silently forwards whatever
// that method does to the underlying workspace - which is worse for a
// mutation, since read-only is a safety property. What closes that gap is
// TestReadOnlyWorkspace_MethodClassificationIsComplete: it reflects over
// Workspace's method set and fails whenever a method is not accounted for
// in either refusedMethods or readOnlySafeMethods (see
// read_only_workspace_classification_test.go), so a new mutating method
// must be classified - and, if refused, given an override here - before
// that test passes again.
type readOnlyWorkspace struct {
	Workspace
	workingDir string
	sessionID  string
	reason     string
}

// readOnlyError returns a typed error for the given operation name.
func (w *readOnlyWorkspace) readOnlyError(op string) error {
	return &ErrReadOnlyOperation{Operation: op, Reason: w.reason}
}

// newReadOnlyWorkspace creates a read-only wrapper over the given workspace.
// workingDir is the thread worktree path (not the parent's workdir).
// reason is why this view is read-only, carried into every refusal this
// workspace returns - see ErrReadOnlyOperation.Reason. Pass "" when there
// is nothing to explain.
func newReadOnlyWorkspace(ws Workspace, workingDir, sessionID, reason string) *readOnlyWorkspace {
	return &readOnlyWorkspace{Workspace: ws, workingDir: workingDir, sessionID: sessionID, reason: reason}
}

// allowsSession permits the thread's root session and genuine agent-tool
// sessions whose persisted parent chain leads back to that root. Parsing is
// required for every descendant so an arbitrary session that merely claims a
// root parent cannot widen this read-only view.
func (w *readOnlyWorkspace) allowsSession(ctx context.Context, id string) (bool, error) {
	if id == w.sessionID {
		return true, nil
	}
	if _, _, ok := w.ParseAgentToolSessionID(id); !ok {
		return false, nil
	}

	seen := map[string]struct{}{id: {}}
	for id != w.sessionID {
		sess, err := w.Workspace.GetSession(ctx, id)
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
		if _, _, ok := w.ParseAgentToolSessionID(parentID); !ok {
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

// -- Sessions --

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
	return w.Workspace.GetSession(ctx, sessionID)
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

// -- Messages --

func (w *readOnlyWorkspace) ListMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	allowed, err := w.allowsSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, w.scopeError(sessionID)
	}
	return w.Workspace.ListMessages(ctx, sessionID)
}

func (w *readOnlyWorkspace) ListUserMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	allowed, err := w.allowsSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, w.scopeError(sessionID)
	}
	return w.Workspace.ListUserMessages(ctx, sessionID)
}

func (w *readOnlyWorkspace) ListAllUserMessages(ctx context.Context) ([]message.Message, error) {
	return w.Workspace.ListUserMessages(ctx, w.sessionID)
}

func (w *readOnlyWorkspace) ListMessagesBySessionIDs(ctx context.Context, rootSessionID string, generation uint64, sessionIDs []string) (map[string][]message.Message, error) {
	if rootSessionID != w.sessionID {
		return nil, w.scopeError(rootSessionID)
	}
	return w.Workspace.ListMessagesBySessionIDs(ctx, w.sessionID, generation, sessionIDs)
}

// -- Agent --

func (w *readOnlyWorkspace) AgentRun(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) error {
	return w.readOnlyError("AgentRun")
}

func (w *readOnlyWorkspace) AgentRunShellCommand(ctx context.Context, sessionID, command string, termWidth int, onProgress func(string), isFirstMessage bool) (proto.ShellCommandResponse, error) {
	return proto.ShellCommandResponse{}, w.readOnlyError("AgentRunShellCommand")
}

func (w *readOnlyWorkspace) AgentCancel(sessionID string) {
	// No-op: cancelling a non-running thread is harmless.
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

// ApplySessionModel is a no-op here rather than an error. This wrapper is
// how a caller inspects a thread it cannot run (see
// AppWorkspace.AttachThread), and the instance it would switch is the
// parent's, not the thread's: looking at a thread must not change the
// model the user's own session runs on.
func (w *readOnlyWorkspace) ApplySessionModel(context.Context, string) (bool, error) {
	return false, nil
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

// -- FileTracker --

func (w *readOnlyWorkspace) FileTrackerRecordRead(ctx context.Context, sessionID, path string) {
	// No-op: recording reads is harmless.
}

func (w *readOnlyWorkspace) FileTrackerLastReadTime(ctx context.Context, sessionID, path string) time.Time {
	allowed, err := w.allowsSession(ctx, sessionID)
	if err != nil || !allowed {
		return time.Time{}
	}
	return w.Workspace.FileTrackerLastReadTime(ctx, sessionID, path)
}

func (w *readOnlyWorkspace) FileTrackerListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	allowed, err := w.allowsSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, w.scopeError(sessionID)
	}
	return w.Workspace.FileTrackerListReadFiles(ctx, sessionID)
}

// -- History --

func (w *readOnlyWorkspace) ListSessionHistory(ctx context.Context, sessionID string) ([]history.File, error) {
	allowed, err := w.allowsSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, w.scopeError(sessionID)
	}
	return w.Workspace.ListSessionHistory(ctx, sessionID)
}

// -- LSP (mutations only) --

func (w *readOnlyWorkspace) LSPStart(ctx context.Context, path string) {
	// No-op: starting LSP on a read-only workspace is harmless.
}

func (w *readOnlyWorkspace) LSPStopAll(ctx context.Context) {
	// No-op: stopping LSP on a read-only workspace is harmless.
}

// -- Config --

func (w *readOnlyWorkspace) WorkingDir() string {
	return w.workingDir
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

// -- Project lifecycle (mutations only) --

func (w *readOnlyWorkspace) MarkProjectInitialized() error {
	return w.readOnlyError("MarkProjectInitialized")
}

// -- MCP (mutations only) --

func (w *readOnlyWorkspace) MCPRefreshPrompts(ctx context.Context, name string) {
	// No-op: refreshing prompts on a read-only workspace is harmless.
}

func (w *readOnlyWorkspace) MCPRefreshResources(ctx context.Context, name string) {
	// No-op: refreshing resources on a read-only workspace is harmless.
}

func (w *readOnlyWorkspace) RefreshMCPTools(ctx context.Context, name string) {
	// No-op: refreshing tools on a read-only workspace is harmless.
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

// -- ThreadController (mutations only) --

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

// -- TaskController (mutations only) --

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
