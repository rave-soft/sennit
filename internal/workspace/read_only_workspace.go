package workspace

import (
	"context"
	"errors"
	"fmt"
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
	return fmt.Sprintf("read-only workspace: %s is not allowed: %s", e.Operation, e.Reason)
}

// readOnlyWorkspace wraps an underlying Workspace and restricts all
// mutating operations. It is used for completed/interrupted threads
// so the TUI can inspect persisted session data without risking
// accidental writes or tearing down the parent workspace.
//
// It holds the wrapped Workspace in an unexported field (ws) rather than
// embedding it, so nothing is promoted: every method Workspace declares
// must be implemented explicitly below, or the compile-time assertion
// `var _ Workspace = (*readOnlyWorkspace)(nil)` at the bottom of this file
// fails. That is a default-deny design - a method Workspace grows that
// this type does not yet implement is a compile error, not a method that
// silently forwards to the wrapped workspace's real, possibly mutating,
// state. Most of the explicit methods below refuse a mutation outright;
// the rest ("-- Safe pass-through reads --") are pure reads that simply
// forward to w.ws, spelled out here instead of inherited so a future
// mutating method can never join them by accident.
//
// TestReadOnlyWorkspace_MethodClassificationIsComplete (see
// read_only_workspace_classification_test.go) still reflects over
// Workspace's method set and requires every method to be listed in either
// refusedMethods or readOnlySafeMethods. The compiler now guarantees
// completeness on its own, but the classification test remains valuable as
// living documentation of which methods were deliberately judged safe to
// read through versus refused, and TestReadOnlyWorkspace_RefusesEveryMutatingMethod
// still proves the refusals behave correctly rather than merely compile.
type readOnlyWorkspace struct {
	ws         Workspace
	workingDir string
	sessionID  string
	reason     string
	// uncommittedFiles answers UncommittedFiles for workingDir. It is
	// supplied by the caller (see NewReadOnlyWorkspace) rather than this
	// package calling git.UncommittedFiles itself, so the subprocess this
	// launches stays out of the Workspace contract package: appws, the
	// only production caller, passes git.UncommittedFiles directly.
	uncommittedFiles func(ctx context.Context, dir string) ([]git.FileChange, error)
}

// readOnlyError returns a typed error for the given operation name.
func (w *readOnlyWorkspace) readOnlyError(op string) error {
	return &ErrReadOnlyOperation{Operation: op, Reason: w.reason}
}

// NewReadOnlyWorkspace creates a read-only wrapper over the given workspace.
// workingDir is the thread worktree path (not the parent's workdir).
// reason is why this view is read-only, carried into every refusal this
// workspace returns - see ErrReadOnlyOperation.Reason. Pass "" when there
// is nothing to explain.
//
// Exported (rather than a private helper local to one caller) because
// internal/workspace/appws builds one of these for a thread whose
// workspace is not currently spawned (see AppWorkspace.AttachThread);
// the returned *readOnlyWorkspace stays unexported since callers only
// ever need it through the Workspace interface it satisfies.
//
// uncommittedFiles is the function this wrapper's own UncommittedFiles
// delegates to, scoped to workingDir; production callers pass
// git.UncommittedFiles.
func NewReadOnlyWorkspace(
	ws Workspace,
	workingDir, sessionID, reason string,
	uncommittedFiles func(ctx context.Context, dir string) ([]git.FileChange, error),
) *readOnlyWorkspace {
	return &readOnlyWorkspace{
		ws: ws, workingDir: workingDir, sessionID: sessionID, reason: reason,
		uncommittedFiles: uncommittedFiles,
	}
}

// allowsSession permits the thread's root session and genuine agent-tool
// sessions whose persisted parent chain leads back to that root. Parsing is
// required for every descendant so an arbitrary session that merely claims a
// root parent cannot widen this read-only view.
func (w *readOnlyWorkspace) allowsSession(ctx context.Context, id string) (bool, error) {
	if id == w.sessionID {
		return true, nil
	}
	if _, _, ok := session.ParseAgentToolSessionID(id); !ok {
		return false, nil
	}

	seen := map[string]struct{}{id: {}}
	for id != w.sessionID {
		sess, err := w.ws.GetSession(ctx, id)
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
		if _, _, ok := session.ParseAgentToolSessionID(parentID); !ok {
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
	return w.ws.GetSession(ctx, sessionID)
}

func (w *readOnlyWorkspace) ListSessions(ctx context.Context) ([]session.Session, error) {
	sess, err := w.GetSession(ctx, w.sessionID)
	if err != nil {
		return nil, err
	}
	return []session.Session{sess}, nil
}

// GetLastSession must not forward to the embedded Workspace: that
// workspace is the parent AppWorkspace (see AttachThread's read-only
// fallback), and its GetLastSession answers "the most recently updated
// top-level session in the parent's project" - some other thread, or the
// parent's own session, never this one. A read-only thread view has
// exactly one top-level session, already known as w.sessionID, so that is
// what it reports.
func (w *readOnlyWorkspace) GetLastSession(ctx context.Context) (session.Session, error) {
	return w.GetSession(ctx, w.sessionID)
}

func (w *readOnlyWorkspace) RenameSession(ctx context.Context, sessionID string, title string) error {
	return w.readOnlyError("RenameSession")
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

func (w *readOnlyWorkspace) SessionDescendantCost(ctx context.Context, sessionID string) (float64, error) {
	allowed, err := w.allowsSession(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	if !allowed {
		return 0, w.scopeError(sessionID)
	}
	return w.ws.SessionDescendantCost(ctx, sessionID)
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
	return w.ws.ListMessages(ctx, sessionID)
}

func (w *readOnlyWorkspace) ListUserMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	allowed, err := w.allowsSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, w.scopeError(sessionID)
	}
	return w.ws.ListUserMessages(ctx, sessionID)
}

func (w *readOnlyWorkspace) ListAllUserMessages(ctx context.Context) ([]message.Message, error) {
	return w.ws.ListUserMessages(ctx, w.sessionID)
}

func (w *readOnlyWorkspace) ListMessagesBySessionIDs(ctx context.Context, rootSessionID string, generation uint64, sessionIDs []string) (map[string][]message.Message, error) {
	if rootSessionID != w.sessionID {
		return nil, w.scopeError(rootSessionID)
	}
	for _, sessionID := range sessionIDs {
		allowed, err := w.allowsSession(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		if !allowed {
			return nil, w.scopeError(sessionID)
		}
	}
	return w.ws.ListMessagesBySessionIDs(ctx, w.sessionID, generation, sessionIDs)
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

func (w *readOnlyWorkspace) AgentRunStream(ctx context.Context, sessionID, prompt string, opts AgentRunOptions) (<-chan AgentRunEvent, error) {
	return nil, w.readOnlyError("AgentRunStream")
}

// ResetAgentToolCache proxies to the wrapped workspace rather than
// no-opping like the other mutations above: it clears a process-wide
// cache, not anything scoped to a session or to write access, so there is
// no read-only boundary for it to respect, and no-opping here would
// silently stop the cache from being cleared whenever the caller happens
// to be holding a read-only view (e.g. while inspecting a thread) instead
// of the underlying workspace.
func (w *readOnlyWorkspace) ResetAgentToolCache() {
	w.ws.ResetAgentToolCache()
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

func (w *readOnlyWorkspace) QuestionAnswer(batchID string, responses []question.Answer) bool {
	return false
}

func (w *readOnlyWorkspace) QuestionCancel() bool {
	return false
}

// PrepareSessionChanges must not delegate to the embedded Workspace's own
// PrepareSessionChanges: that method (AppWorkspace's) closes over its own
// UncommittedFiles, which reads the parent's working directory - not this
// wrapper's overridden one. Calling it here would compute the thread's
// file diff against the parent's repository, marking every file the
// thread touched as "uncommitted" relative to a tree it does not belong
// to. Using this wrapper's own ListSessionHistory and UncommittedFiles
// keeps everything scoped to the thread's own worktree.
func (w *readOnlyWorkspace) PrepareSessionChanges(ctx context.Context, sessionID string) ([]SessionFile, error) {
	allowed, err := w.allowsSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, w.scopeError(sessionID)
	}
	return PrepareSessionChangesUsing(ctx, sessionID, w.ListSessionHistory, w.UncommittedFiles)
}

// UncommittedFiles must not forward to the embedded Workspace: that
// workspace is the parent AppWorkspace, and its UncommittedFiles diffs the
// parent's own working directory, not this thread's worktree. w.workingDir
// is the thread's worktree path (see NewReadOnlyWorkspace), so that is
// what gets diffed here.
func (w *readOnlyWorkspace) UncommittedFiles(ctx context.Context) ([]git.FileChange, error) {
	return w.uncommittedFiles(ctx, w.workingDir)
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
	return w.ws.FileTrackerLastReadTime(ctx, sessionID, path)
}

func (w *readOnlyWorkspace) FileTrackerListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	allowed, err := w.allowsSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, w.scopeError(sessionID)
	}
	return w.ws.FileTrackerListReadFiles(ctx, sessionID)
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
	return w.ws.ListSessionHistory(ctx, sessionID)
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

// VerifyProviderAPIKey is refused rather than passed through, for the same
// reason RefreshAccountLimits is below: although the probe itself writes
// nothing, it is a live network call made in the parent workspace's name,
// and a read-only thread view exists to let a completed/interrupted thread
// be inspected without acting as the workspace it is attached to.
func (w *readOnlyWorkspace) VerifyProviderAPIKey(ctx context.Context, providerID, apiKey string) error {
	return w.readOnlyError("VerifyProviderAPIKey")
}

// ConfigureCustomProvider is refused rather than passed through, for the
// same reason VerifyProviderAPIKey above is: it makes a live network call
// (model discovery) and writes config, both in the parent workspace's
// name, and a read-only thread view exists to let a completed/interrupted
// thread be inspected without acting as the workspace it is attached to.
func (w *readOnlyWorkspace) ConfigureCustomProvider(ctx context.Context, scope config.Scope, params ConfigureCustomProviderParams) ([]catwalk.Model, error) {
	return nil, w.readOnlyError("ConfigureCustomProvider")
}

func (w *readOnlyWorkspace) RecordAccount(scope config.Scope, providerID string, cred accounts.LegacyCredential) (accounts.Account, error) {
	return accounts.Account{}, w.readOnlyError("RecordAccount")
}

func (w *readOnlyWorkspace) ActivateAccount(scope config.Scope, providerID, accountID string) error {
	return w.readOnlyError("ActivateAccount")
}

func (w *readOnlyWorkspace) UpdateAccount(providerID string, account accounts.Account) error {
	return w.readOnlyError("UpdateAccount")
}

func (w *readOnlyWorkspace) RemoveAccount(scope config.Scope, providerID, accountID string) error {
	return w.readOnlyError("RemoveAccount")
}

func (w *readOnlyWorkspace) PurgeAccounts(scope config.Scope, providerID string) error {
	return w.readOnlyError("PurgeAccounts")
}

func (w *readOnlyWorkspace) SetProviderProxy(providerID, proxy string) error {
	return w.readOnlyError("SetProviderProxy")
}

func (w *readOnlyWorkspace) RefreshAccountLimits(ctx context.Context, providerID string) ([]accounts.Account, error) {
	return nil, w.readOnlyError("RefreshAccountLimits")
}

// StartOAuth and CompleteOAuth are refused for the same reason
// RefreshAccountLimits and VerifyProviderAPIKey are: both are live network
// calls made — and CompleteOAuth persists a credential — in the parent
// workspace's name, which a read-only thread view exists to avoid doing.
func (w *readOnlyWorkspace) StartOAuth(ctx context.Context, providerID, proxyURL string) (OAuthStartResult, OAuthFlow, error) {
	return OAuthStartResult{}, nil, w.readOnlyError("StartOAuth")
}

func (w *readOnlyWorkspace) CompleteOAuth(ctx context.Context, providerID, proxyURL string, token *oauth.Token, forceNewAccount bool) (OAuthCompletion, error) {
	return OAuthCompletion{}, w.readOnlyError("CompleteOAuth")
}

// OAuthConfiguredProxy and OAuthValidateProxy are pure reads/validation
// with no side effect and no network call, so they pass through like the
// rest of "-- Safe pass-through reads --" below.
func (w *readOnlyWorkspace) OAuthConfiguredProxy(providerID string) string {
	return w.ws.OAuthConfiguredProxy(providerID)
}

func (w *readOnlyWorkspace) OAuthValidateProxy(providerID, proxyURL string) error {
	return w.ws.OAuthValidateProxy(providerID, proxyURL)
}

// CurrentPlanUsage is a read, so the read-only workspace answers it: an
// attached thread shows the same plan line as the workspace it is attached
// to.
func (w *readOnlyWorkspace) DockerMCPAvailable() (available, known bool) {
	return w.ws.DockerMCPAvailable()
}

// RefreshDockerMCPAvailability runs a probe and caches its answer, but it
// changes nothing about this workspace or the project — it is a question
// about the machine — so a read-only workspace may ask it.
func (w *readOnlyWorkspace) RefreshDockerMCPAvailability() bool {
	return w.ws.RefreshDockerMCPAvailability()
}

func (w *readOnlyWorkspace) KnownProviders() []catwalk.Provider {
	return w.ws.KnownProviders()
}

func (w *readOnlyWorkspace) CustomProviderTypes() []string {
	return w.ws.CustomProviderTypes()
}

func (w *readOnlyWorkspace) CurrentPlanUsage(providerID string) (accounts.Usage, bool) {
	return w.ws.CurrentPlanUsage(providerID)
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
func SupportsThreadAttach(ws ThreadController) bool {
	_, refuses := ws.(threadAttachRefuser)
	return !refuses
}

// -- TaskController (mutations only) --

func (w *readOnlyWorkspace) CancelTask(ctx context.Context, id, reason string) error {
	return w.readOnlyError("CancelTask")
}

// -- EventSubscriber --

func (w *readOnlyWorkspace) Subscribe(send func(any)) {
	// No-op: subscribing to a read-only workspace's events is safe
	// but not meaningful for a completed thread.
}

func (w *readOnlyWorkspace) Shutdown() {
	// No-op: shutting down a read-only workspace must NOT affect
	// the parent workspace. It is safe to call multiple times.
}

// -- Safe pass-through reads --
//
// Everything below is a pure read with no thread-scoping of its own to
// apply, forwarded to w.ws unchanged. These used to be inherited for free
// by embedding Workspace; they are spelled out individually now so that
// readOnlyWorkspace's method set is exactly what this file says it is, and
// nothing more (see the struct's doc comment above).

func (w *readOnlyWorkspace) AgentIsBusy() bool {
	return w.ws.AgentIsBusy()
}

func (w *readOnlyWorkspace) AgentIsReady() bool {
	return w.ws.AgentIsReady()
}

func (w *readOnlyWorkspace) AgentIsSessionBusy(sessionID string) bool {
	return w.ws.AgentIsSessionBusy(sessionID)
}

func (w *readOnlyWorkspace) AgentModel() AgentModel {
	return w.ws.AgentModel()
}

func (w *readOnlyWorkspace) AgentQueuedPromptsList(sessionID string) []string {
	return w.ws.AgentQueuedPromptsList(sessionID)
}

func (w *readOnlyWorkspace) AgentReadyErr() error {
	return w.ws.AgentReadyErr()
}

func (w *readOnlyWorkspace) BackgroundJobCounts() BackgroundJobCounts {
	return w.ws.BackgroundJobCounts()
}

// AccountCapabilities is a read: it reports what a provider supports, and
// touches no account state.
func (w *readOnlyWorkspace) AccountCapabilities(providerID string) AccountCapabilities {
	return w.ws.AccountCapabilities(providerID)
}

func (w *readOnlyWorkspace) Config() *config.Config {
	return w.ws.Config()
}

func (w *readOnlyWorkspace) GetMCPPrompt(clientID, promptID string, args map[string]string) (string, error) {
	return w.ws.GetMCPPrompt(clientID, promptID, args)
}

func (w *readOnlyWorkspace) GetThread(ctx context.Context, id string) (proto.Thread, error) {
	return w.ws.GetThread(ctx, id)
}

func (w *readOnlyWorkspace) InitializePrompt() (string, error) {
	return w.ws.InitializePrompt()
}

func (w *readOnlyWorkspace) LSPGetDiagnosticCounts(name string) proto.LSPDiagnosticCounts {
	return w.ws.LSPGetDiagnosticCounts(name)
}

func (w *readOnlyWorkspace) LSPGetStates() map[string]LSPClientInfo {
	return w.ws.LSPGetStates()
}

func (w *readOnlyWorkspace) ListAccounts(providerID string) ([]accounts.Account, error) {
	return w.ws.ListAccounts(providerID)
}

func (w *readOnlyWorkspace) ListMCPPrompts(ctx context.Context) ([]MCPPrompt, error) {
	return w.ws.ListMCPPrompts(ctx)
}

func (w *readOnlyWorkspace) ListSkills(ctx context.Context) ([]skills.CatalogEntry, error) {
	return w.ws.ListSkills(ctx)
}

func (w *readOnlyWorkspace) ConfigProblems() []config.Problem {
	return w.ws.ConfigProblems()
}

func (w *readOnlyWorkspace) SkillStates() []*skills.SkillState {
	return w.ws.SkillStates()
}

func (w *readOnlyWorkspace) BuiltinSkills() []*skills.Skill {
	return w.ws.BuiltinSkills()
}

func (w *readOnlyWorkspace) DoctorProblems() []config.Problem {
	return w.ws.DoctorProblems()
}

func (w *readOnlyWorkspace) ListCustomCommands(ctx context.Context) ([]CustomCommand, error) {
	return w.ws.ListCustomCommands(ctx)
}

func (w *readOnlyWorkspace) ListTasks(ctx context.Context) ([]proto.Thread, error) {
	return w.ws.ListTasks(ctx)
}

func (w *readOnlyWorkspace) ListThreads(ctx context.Context) ([]proto.Thread, error) {
	return w.ws.ListThreads(ctx)
}

func (w *readOnlyWorkspace) MCPAuthURL(name string) string {
	return w.ws.MCPAuthURL(name)
}

func (w *readOnlyWorkspace) MCPGetStates() map[string]MCPClientInfo {
	return w.ws.MCPGetStates()
}

func (w *readOnlyWorkspace) MCPPendingAuth() []MCPPendingAuthServer {
	return w.ws.MCPPendingAuth()
}

func (w *readOnlyWorkspace) MCPResources() []MCPResourceInfo {
	return w.ws.MCPResources()
}

func (w *readOnlyWorkspace) PermissionSkipRequests() bool {
	return w.ws.PermissionSkipRequests()
}

func (w *readOnlyWorkspace) ProjectNeedsInitialization() (bool, error) {
	return w.ws.ProjectNeedsInitialization()
}

func (w *readOnlyWorkspace) ReadMCPResource(ctx context.Context, name, uri string) ([]MCPResourceContents, error) {
	return w.ws.ReadMCPResource(ctx, name, uri)
}

func (w *readOnlyWorkspace) ReadSkill(ctx context.Context, skillID string) ([]byte, skills.SkillReadResult, error) {
	return w.ws.ReadSkill(ctx, skillID)
}

func (w *readOnlyWorkspace) Resolver() config.VariableResolver {
	return w.ws.Resolver()
}

func (w *readOnlyWorkspace) Stats(ctx context.Context, req stats.Request) (stats.Snapshot, error) {
	return w.ws.Stats(ctx, req)
}

func (w *readOnlyWorkspace) SupportsTasks() bool {
	return w.ws.SupportsTasks()
}

func (w *readOnlyWorkspace) SupportsThreads() bool {
	return w.ws.SupportsThreads()
}

func (w *readOnlyWorkspace) WaitForMCPInit(ctx context.Context) error {
	return w.ws.WaitForMCPInit(ctx)
}

// Compile-time check that readOnlyWorkspace implements Workspace.
var _ Workspace = (*readOnlyWorkspace)(nil)

// IsReadOnlyError reports whether err is a read-only operation error.
func IsReadOnlyError(err error) bool {
	var roErr *ErrReadOnlyOperation
	return err != nil && errors.As(err, &roErr)
}
