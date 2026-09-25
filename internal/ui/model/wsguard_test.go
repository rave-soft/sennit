package model

// Guard against synchronous U/S/H Workspace calls made on the Bubble Tea
// Update goroutine (Update, View/Draw, New, and anything they call
// directly, including dialog constructors) -- see CLIENT-SERVER.md's
// "Классы методов" and "Синхронные вызовы из Update/View". Once
// workspace.Workspace is served over gRPC, any such call becomes a network
// round trip; making it synchronously there freezes the whole TUI for as
// long as the call (or a dropped connection) takes.

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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
	"github.com/rave-soft/sennit/internal/workspace"
)

// updateGoroutineGuardedMethods is a duplicate of the U/S/H entries in
// internal/workspace/wire_classes_test.go's methodClasses (everything that
// is not classCachedGetter "C" or classClientLocal "X"). It has to be a
// duplicate rather than an import: that table lives in a _test.go file, and
// _test.go symbols are only linked into their own package's test binary --
// internal/ui/model cannot import them, the same way internal/workspace's
// own tests cannot import this file's table (recall commit 59666e391,
// which forbade putting a test-only table in a production file just to
// make it importable both ways).
//
// Instead, internal/workspace/wire_classes_ui_guard_test.go reads this
// file's source as text and compares the two lists, failing loudly on
// drift in either direction: a method added to methodClasses here without
// a matching entry here, or vice versa.
var updateGoroutineGuardedMethods = map[string]bool{
	"ActivateAccount":              true,
	"ActivateThread":               true,
	"AgentCancel":                  true,
	"AgentClearQueue":              true,
	"AgentRun":                     true,
	"AgentRunShellCommand":         true,
	"AgentRunStream":               true,
	"AgentSummarize":               true,
	"ApplySessionModel":            true,
	"AttachThread":                 true,
	"BuiltinSkills":                true,
	"CancelTask":                   true,
	"CancelThread":                 true,
	"CompleteOAuth":                true,
	"ConfigProblems":               true,
	"ConfigureCustomProvider":      true,
	"CreateSession":                true,
	"CreateThread":                 true,
	"DeleteSession":                true,
	"DisableDockerMCP":             true,
	"DoctorProblems":               true,
	"EnableDockerMCP":              true,
	"EnterWorktree":                true,
	"ExitWorktree":                 true,
	"FileTrackerLastReadTime":      true,
	"FileTrackerListReadFiles":     true,
	"FileTrackerRecordRead":        true,
	"GetLastSession":               true,
	"GetMCPPrompt":                 true,
	"GetSession":                   true,
	"ImportCopilot":                true,
	"InitCoderAgent":               true,
	"InitCoderAgentNonInteractive": true,
	"InitializePrompt":             true,
	"ListAccounts":                 true,
	"ListAllUserMessages":          true,
	"ListCustomCommands":           true,
	"ListMCPPrompts":               true,
	"ListMessages":                 true,
	"ListMessagesBySessionIDs":     true,
	"ListSessionHistory":           true,
	"ListSessions":                 true,
	"ListSkills":                   true,
	"ListTasks":                    true,
	"ListThreads":                  true,
	"ListUserMessages":             true,
	"LSPGetDiagnosticCounts":       true,
	"LSPGetStates":                 true,
	"LSPStart":                     true,
	"LSPStopAll":                   true,
	"MarkProjectInitialized":       true,
	"MCPAuthenticate":              true,
	"MCPGetStates":                 true,
	"MCPRefreshPrompts":            true,
	"MCPRefreshResources":          true,
	"MCPResources":                 true,
	"OAuthConfiguredProxy":         true,
	"OAuthValidateProxy":           true,
	"OverridePreferredModel":       true,
	"PermissionDeny":               true,
	"PermissionGrant":              true,
	"PermissionGrantPersistent":    true,
	"PermissionSetSkipRequests":    true,
	"ProjectNeedsInitialization":   true,
	"PurgeAccounts":                true,
	"QuestionAnswer":               true,
	"QuestionCancel":               true,
	"ReadMCPResource":              true,
	"ReadSkill":                    true,
	"RecordAccount":                true,
	"RefreshAccountLimits":         true,
	"RefreshDockerMCPAvailability": true,
	"RefreshMCPTools":              true,
	"RefreshOAuthToken":            true,
	"RefreshOAuthTokenForAccount":  true,
	"RefreshProviderModels":        true,
	"RemoveAccount":                true,
	"RemoveConfigField":            true,
	"RemoveThread":                 true,
	"RenameSession":                true,
	"SessionDescendantCost":        true,
	"SetConfigField":               true,
	"SetCurrentSession":            true,
	"SetCurrentSessionGeneration":  true,
	"SetProviderAPIKey":            true,
	"SetProviderProxy":             true,
	"SkillStates":                  true,
	"StartOAuth":                   true,
	"Stats":                        true,
	"Subscribe":                    true,
	"SubscribeWith":                true,
	"UncommittedFiles":             true,
	"UpdateAccountFields":          true,
	"UpdateAgentModel":             true,
	"UpdatePreferredModel":         true,
	"VerifyProviderAPIKey":         true,
	"WaitForMCPInit":               true,
}

// updateGoroutineGuard wraps a workspace.Workspace and fails the enclosing
// test the moment a guarded (U/S/H) method runs while onUpdateGoroutine is
// set. The flag is a *atomic.Bool so a test can flip it from the driving
// harness (see setUpdateGoroutine/clearUpdateGoroutine in
// command_driving_test.go) without racing a tea.Cmd goroutine that is
// still finishing up when the next Update call starts.
type updateGoroutineGuard struct {
	workspace.Workspace
	t  *testing.T
	on *atomic.Bool
}

// newUpdateGoroutineGuard wraps ws for t, guarding against synchronous
// U/S/H calls while on reports true. on is shared with the driving harness
// so several call sites (Update, View, New) can all guard against the same
// flag.
func newUpdateGoroutineGuard(t *testing.T, ws workspace.Workspace, on *atomic.Bool) workspace.Workspace {
	t.Helper()
	return &updateGoroutineGuard{Workspace: ws, t: t, on: on}
}

// check fails the test with the offending method name and a short stack
// excerpt when called while g.on is set. It does not panic or block the
// call -- the real method still runs, so a test keeps whatever behavior it
// was asserting on, and the failure is reported the normal way through
// t.Errorf.
// extraGuardedMethods covers guard methods with no entry in
// updateGoroutineGuardedMethods because they aren't Workspace methods at
// all — PrepareSessionChanges is optional (workspace.SessionChangePreparer,
// resolved by root.go's own type assertion), so it's outside
// wire_classes_test.go's methodClasses and TestUIGuardMethodsMatchWireClasses'
// reach. Kept separate so that table stays an exact mirror of the U/S/H set.
var extraGuardedMethods = map[string]bool{
	"PrepareSessionChanges": true,
}

func (g *updateGoroutineGuard) check(method string) {
	// Every generated wrapper below must name itself here — this is what
	// keeps updateGoroutineGuardedMethods live for
	// TestUIGuardMethodsMatchWireClasses (wire_classes_ui_guard_test.go)
	// to parse, rather than a table nothing in this package ever reads.
	if !updateGoroutineGuardedMethods[method] && !extraGuardedMethods[method] {
		g.t.Fatalf("check(%q): not in updateGoroutineGuardedMethods or extraGuardedMethods; the generated wrapper and the table have drifted", method)
	}
	if !g.on.Load() {
		return
	}
	g.t.Helper()
	stack := debug.Stack()
	if len(stack) > 800 {
		stack = stack[:800]
	}
	g.t.Errorf(
		"workspace.%s called synchronously on the Update goroutine; "+
			"once Workspace is served over gRPC this becomes a network round "+
			"trip and would freeze the UI -- move it into a tea.Cmd\n%s",
		method, stack,
	)
}

// PrepareSessionChanges implements workspace.SessionChangePreparer,
// forwarding to the wrapped workspace when it implements the optional
// interface too. It cannot be part of the generated block below:
// SessionChangePreparer is not a Workspace method (root.go resolves it
// with its own type assertion, msg.ws.(workspace.SessionChangePreparer)),
// so embedding alone would not satisfy it and the assertion would silently
// see "unsupported" for any guarded workspace, the same way it would for a
// real gRPC client stub that never implements this local-only interface.
func (g *updateGoroutineGuard) PrepareSessionChanges(ctx context.Context, sessionID string) ([]workspace.SessionFile, error) {
	preparer, ok := g.Workspace.(workspace.SessionChangePreparer)
	if !ok {
		return nil, fmt.Errorf("workspace does not implement SessionChangePreparer")
	}
	g.check("PrepareSessionChanges")
	return preparer.PrepareSessionChanges(ctx, sessionID)
}

// uiGuardFlags maps a *UI built through the guarded test harness back to
// its shared on/off flag. Keyed by pointer rather than carried as a field
// on UI itself: the flag is scaffolding for the harness, not state real
// production code should ever see or need to zero-value correctly.
var uiGuardFlags sync.Map

// registerGuardFlag associates m with the flag its guarded workspace reads.
// Called once, by whatever test constructor builds a guarded UI.
func registerGuardFlag(m *UI, on *atomic.Bool) {
	uiGuardFlags.Store(m, on)
}

// guardFlagFor looks up the flag registered for m, or nil if m was built
// without the guard (e.g. the many tests that construct &UI{} directly) --
// callers must treat nil as "unguarded" rather than panic, so ungated
// tests keep working unchanged.
func guardFlagFor(m *UI) *atomic.Bool {
	v, ok := uiGuardFlags.Load(m)
	if !ok {
		return nil
	}
	return v.(*atomic.Bool)
}

// runGuardedCmd executes cmd with the guard cleared, mirroring how the real
// Bubble Tea runtime runs a returned tea.Cmd off the Update goroutine.
// Every test call site that invokes a tea.Cmd's closure directly --
// including driveCmdStep and runCmdTree in command_driving_test.go --
// goes through this instead of calling cmd() bare, so the guard does not
// mistake a command's own body (which is allowed to call U/S/H methods)
// for a synchronous call on Update.
func runGuardedCmd(m *UI, cmd tea.Cmd) tea.Msg {
	flag := guardFlagFor(m)
	if flag == nil {
		return cmd()
	}
	flag.Store(false)
	defer flag.Store(true)
	return cmd()
}

func (g *updateGoroutineGuard) ActivateAccount(scope config.Scope, providerID string, accountID string) error {
	g.check("ActivateAccount")
	return g.Workspace.ActivateAccount(scope, providerID, accountID)
}

func (g *updateGoroutineGuard) ActivateThread(ctx context.Context, id string) (proto.Thread, error) {
	g.check("ActivateThread")
	return g.Workspace.ActivateThread(ctx, id)
}

func (g *updateGoroutineGuard) AgentCancel(sessionID string) error {
	g.check("AgentCancel")
	return g.Workspace.AgentCancel(sessionID)
}

func (g *updateGoroutineGuard) AgentClearQueue(sessionID string) error {
	g.check("AgentClearQueue")
	return g.Workspace.AgentClearQueue(sessionID)
}

func (g *updateGoroutineGuard) AgentRun(ctx context.Context, sessionID string, prompt string, attachments ...message.Attachment) error {
	g.check("AgentRun")
	return g.Workspace.AgentRun(ctx, sessionID, prompt, attachments...)
}

func (g *updateGoroutineGuard) AgentRunShellCommand(ctx context.Context, sessionID string, command string, termWidth int, onProgress func(string), isFirstMessage bool) (proto.ShellCommandResponse, error) {
	g.check("AgentRunShellCommand")
	return g.Workspace.AgentRunShellCommand(ctx, sessionID, command, termWidth, onProgress, isFirstMessage)
}

func (g *updateGoroutineGuard) AgentRunStream(ctx context.Context, sessionID string, prompt string, opts workspace.AgentRunOptions) (<-chan workspace.AgentRunEvent, error) {
	g.check("AgentRunStream")
	return g.Workspace.AgentRunStream(ctx, sessionID, prompt, opts)
}

func (g *updateGoroutineGuard) AgentSummarize(ctx context.Context, sessionID string) error {
	g.check("AgentSummarize")
	return g.Workspace.AgentSummarize(ctx, sessionID)
}

func (g *updateGoroutineGuard) ApplySessionModel(ctx context.Context, sessionID string) (bool, error) {
	g.check("ApplySessionModel")
	return g.Workspace.ApplySessionModel(ctx, sessionID)
}

func (g *updateGoroutineGuard) AttachThread(ctx context.Context, id string) (workspace.Workspace, func(), error) {
	g.check("AttachThread")
	return g.Workspace.AttachThread(ctx, id)
}

func (g *updateGoroutineGuard) BuiltinSkills() []*skills.Skill {
	g.check("BuiltinSkills")
	return g.Workspace.BuiltinSkills()
}

func (g *updateGoroutineGuard) CancelTask(ctx context.Context, id string, reason string) error {
	g.check("CancelTask")
	return g.Workspace.CancelTask(ctx, id, reason)
}

func (g *updateGoroutineGuard) CancelThread(ctx context.Context, id string, reason string) error {
	g.check("CancelThread")
	return g.Workspace.CancelThread(ctx, id, reason)
}

func (g *updateGoroutineGuard) CompleteOAuth(ctx context.Context, providerID string, proxyURL string, token *oauth.Token, forceNewAccount bool) (workspace.OAuthCompletion, error) {
	g.check("CompleteOAuth")
	return g.Workspace.CompleteOAuth(ctx, providerID, proxyURL, token, forceNewAccount)
}

func (g *updateGoroutineGuard) ConfigProblems() []config.Problem {
	g.check("ConfigProblems")
	return g.Workspace.ConfigProblems()
}

func (g *updateGoroutineGuard) ConfigureCustomProvider(ctx context.Context, scope config.Scope, params workspace.ConfigureCustomProviderParams) ([]catwalk.Model, error) {
	g.check("ConfigureCustomProvider")
	return g.Workspace.ConfigureCustomProvider(ctx, scope, params)
}

func (g *updateGoroutineGuard) CreateSession(ctx context.Context, title string) (session.Session, error) {
	g.check("CreateSession")
	return g.Workspace.CreateSession(ctx, title)
}

func (g *updateGoroutineGuard) CreateThread(ctx context.Context, req proto.CreateThreadRequest) (proto.Thread, error) {
	g.check("CreateThread")
	return g.Workspace.CreateThread(ctx, req)
}

func (g *updateGoroutineGuard) DeleteSession(ctx context.Context, sessionID string) error {
	g.check("DeleteSession")
	return g.Workspace.DeleteSession(ctx, sessionID)
}

func (g *updateGoroutineGuard) DisableDockerMCP() error {
	g.check("DisableDockerMCP")
	return g.Workspace.DisableDockerMCP()
}

func (g *updateGoroutineGuard) DoctorProblems() []config.Problem {
	g.check("DoctorProblems")
	return g.Workspace.DoctorProblems()
}

func (g *updateGoroutineGuard) EnableDockerMCP(ctx context.Context) error {
	g.check("EnableDockerMCP")
	return g.Workspace.EnableDockerMCP(ctx)
}

func (g *updateGoroutineGuard) EnterWorktree(ctx context.Context, name string) (workspace.Workspace, func(), error) {
	g.check("EnterWorktree")
	return g.Workspace.EnterWorktree(ctx, name)
}

func (g *updateGoroutineGuard) ExitWorktree(ctx context.Context) (workspace.Workspace, func(), error) {
	g.check("ExitWorktree")
	return g.Workspace.ExitWorktree(ctx)
}

func (g *updateGoroutineGuard) FileTrackerLastReadTime(ctx context.Context, sessionID string, path string) (time.Time, error) {
	g.check("FileTrackerLastReadTime")
	return g.Workspace.FileTrackerLastReadTime(ctx, sessionID, path)
}

func (g *updateGoroutineGuard) FileTrackerListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	g.check("FileTrackerListReadFiles")
	return g.Workspace.FileTrackerListReadFiles(ctx, sessionID)
}

func (g *updateGoroutineGuard) FileTrackerRecordRead(ctx context.Context, sessionID string, path string) error {
	g.check("FileTrackerRecordRead")
	return g.Workspace.FileTrackerRecordRead(ctx, sessionID, path)
}

func (g *updateGoroutineGuard) GetLastSession(ctx context.Context) (session.Session, error) {
	g.check("GetLastSession")
	return g.Workspace.GetLastSession(ctx)
}

func (g *updateGoroutineGuard) GetMCPPrompt(ctx context.Context, clientID string, promptID string, args map[string]string) (string, error) {
	g.check("GetMCPPrompt")
	return g.Workspace.GetMCPPrompt(ctx, clientID, promptID, args)
}

func (g *updateGoroutineGuard) GetSession(ctx context.Context, sessionID string) (session.Session, error) {
	g.check("GetSession")
	return g.Workspace.GetSession(ctx, sessionID)
}

func (g *updateGoroutineGuard) ImportCopilot(ctx context.Context) (*oauth.Token, bool, error) {
	g.check("ImportCopilot")
	return g.Workspace.ImportCopilot(ctx)
}

func (g *updateGoroutineGuard) InitCoderAgent(ctx context.Context) error {
	g.check("InitCoderAgent")
	return g.Workspace.InitCoderAgent(ctx)
}

func (g *updateGoroutineGuard) InitCoderAgentNonInteractive(ctx context.Context) error {
	g.check("InitCoderAgentNonInteractive")
	return g.Workspace.InitCoderAgentNonInteractive(ctx)
}

func (g *updateGoroutineGuard) InitializePrompt() (string, error) {
	g.check("InitializePrompt")
	return g.Workspace.InitializePrompt()
}

func (g *updateGoroutineGuard) LSPGetDiagnosticCounts(name string) proto.LSPDiagnosticCounts {
	g.check("LSPGetDiagnosticCounts")
	return g.Workspace.LSPGetDiagnosticCounts(name)
}

func (g *updateGoroutineGuard) LSPGetStates() map[string]workspace.LSPClientInfo {
	g.check("LSPGetStates")
	return g.Workspace.LSPGetStates()
}

func (g *updateGoroutineGuard) LSPStart(ctx context.Context, path string) error {
	g.check("LSPStart")
	return g.Workspace.LSPStart(ctx, path)
}

func (g *updateGoroutineGuard) LSPStopAll(ctx context.Context) error {
	g.check("LSPStopAll")
	return g.Workspace.LSPStopAll(ctx)
}

func (g *updateGoroutineGuard) ListAccounts(providerID string) ([]workspace.FrontendAccount, error) {
	g.check("ListAccounts")
	return g.Workspace.ListAccounts(providerID)
}

func (g *updateGoroutineGuard) ListAllUserMessages(ctx context.Context) ([]message.Message, error) {
	g.check("ListAllUserMessages")
	return g.Workspace.ListAllUserMessages(ctx)
}

func (g *updateGoroutineGuard) ListCustomCommands(ctx context.Context) ([]workspace.CustomCommand, error) {
	g.check("ListCustomCommands")
	return g.Workspace.ListCustomCommands(ctx)
}

func (g *updateGoroutineGuard) ListMCPPrompts(ctx context.Context) ([]workspace.MCPPrompt, error) {
	g.check("ListMCPPrompts")
	return g.Workspace.ListMCPPrompts(ctx)
}

func (g *updateGoroutineGuard) ListMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	g.check("ListMessages")
	return g.Workspace.ListMessages(ctx, sessionID)
}

func (g *updateGoroutineGuard) ListMessagesBySessionIDs(ctx context.Context, rootSessionID string, generation uint64, sessionIDs []string) (map[string][]message.Message, error) {
	g.check("ListMessagesBySessionIDs")
	return g.Workspace.ListMessagesBySessionIDs(ctx, rootSessionID, generation, sessionIDs)
}

func (g *updateGoroutineGuard) ListSessionHistory(ctx context.Context, sessionID string) ([]history.File, error) {
	g.check("ListSessionHistory")
	return g.Workspace.ListSessionHistory(ctx, sessionID)
}

func (g *updateGoroutineGuard) ListSessions(ctx context.Context) ([]session.Session, error) {
	g.check("ListSessions")
	return g.Workspace.ListSessions(ctx)
}

func (g *updateGoroutineGuard) ListSkills(ctx context.Context) ([]skills.CatalogEntry, error) {
	g.check("ListSkills")
	return g.Workspace.ListSkills(ctx)
}

func (g *updateGoroutineGuard) ListTasks(ctx context.Context) ([]proto.Thread, error) {
	g.check("ListTasks")
	return g.Workspace.ListTasks(ctx)
}

func (g *updateGoroutineGuard) ListThreads(ctx context.Context) ([]proto.Thread, error) {
	g.check("ListThreads")
	return g.Workspace.ListThreads(ctx)
}

func (g *updateGoroutineGuard) ListUserMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	g.check("ListUserMessages")
	return g.Workspace.ListUserMessages(ctx, sessionID)
}

func (g *updateGoroutineGuard) MCPAuthenticate(ctx context.Context, name string) error {
	g.check("MCPAuthenticate")
	return g.Workspace.MCPAuthenticate(ctx, name)
}

func (g *updateGoroutineGuard) MCPGetStates() map[string]workspace.MCPClientInfo {
	g.check("MCPGetStates")
	return g.Workspace.MCPGetStates()
}

func (g *updateGoroutineGuard) MCPRefreshPrompts(ctx context.Context, name string) error {
	g.check("MCPRefreshPrompts")
	return g.Workspace.MCPRefreshPrompts(ctx, name)
}

func (g *updateGoroutineGuard) MCPRefreshResources(ctx context.Context, name string) error {
	g.check("MCPRefreshResources")
	return g.Workspace.MCPRefreshResources(ctx, name)
}

func (g *updateGoroutineGuard) MCPResources() []workspace.MCPResourceInfo {
	g.check("MCPResources")
	return g.Workspace.MCPResources()
}

func (g *updateGoroutineGuard) MarkProjectInitialized() error {
	g.check("MarkProjectInitialized")
	return g.Workspace.MarkProjectInitialized()
}

func (g *updateGoroutineGuard) OAuthConfiguredProxy(providerID string) string {
	g.check("OAuthConfiguredProxy")
	return g.Workspace.OAuthConfiguredProxy(providerID)
}

func (g *updateGoroutineGuard) OAuthValidateProxy(providerID string, proxyURL string) error {
	g.check("OAuthValidateProxy")
	return g.Workspace.OAuthValidateProxy(providerID, proxyURL)
}

func (g *updateGoroutineGuard) OverridePreferredModel(model config.SelectedModel) error {
	g.check("OverridePreferredModel")
	return g.Workspace.OverridePreferredModel(model)
}

func (g *updateGoroutineGuard) PermissionDeny(perm permission.PermissionRequest) (bool, error) {
	g.check("PermissionDeny")
	return g.Workspace.PermissionDeny(perm)
}

func (g *updateGoroutineGuard) PermissionGrant(perm permission.PermissionRequest) (bool, error) {
	g.check("PermissionGrant")
	return g.Workspace.PermissionGrant(perm)
}

func (g *updateGoroutineGuard) PermissionGrantPersistent(perm permission.PermissionRequest) (bool, error) {
	g.check("PermissionGrantPersistent")
	return g.Workspace.PermissionGrantPersistent(perm)
}

func (g *updateGoroutineGuard) PermissionSetSkipRequests(skip bool) error {
	g.check("PermissionSetSkipRequests")
	return g.Workspace.PermissionSetSkipRequests(skip)
}

func (g *updateGoroutineGuard) ProjectNeedsInitialization() (bool, error) {
	g.check("ProjectNeedsInitialization")
	return g.Workspace.ProjectNeedsInitialization()
}

func (g *updateGoroutineGuard) PurgeAccounts(scope config.Scope, providerID string) error {
	g.check("PurgeAccounts")
	return g.Workspace.PurgeAccounts(scope, providerID)
}

func (g *updateGoroutineGuard) QuestionAnswer(batchID string, responses []question.Answer) (bool, error) {
	g.check("QuestionAnswer")
	return g.Workspace.QuestionAnswer(batchID, responses)
}

func (g *updateGoroutineGuard) QuestionCancel(batchID string) (bool, error) {
	g.check("QuestionCancel")
	return g.Workspace.QuestionCancel(batchID)
}

func (g *updateGoroutineGuard) ReadMCPResource(ctx context.Context, name string, uri string) ([]workspace.MCPResourceContents, error) {
	g.check("ReadMCPResource")
	return g.Workspace.ReadMCPResource(ctx, name, uri)
}

func (g *updateGoroutineGuard) ReadSkill(ctx context.Context, skillID string) ([]byte, skills.SkillReadResult, error) {
	g.check("ReadSkill")
	return g.Workspace.ReadSkill(ctx, skillID)
}

func (g *updateGoroutineGuard) RecordAccount(scope config.Scope, providerID string, cred accounts.LegacyCredential) (workspace.FrontendAccount, error) {
	g.check("RecordAccount")
	return g.Workspace.RecordAccount(scope, providerID, cred)
}

func (g *updateGoroutineGuard) RefreshAccountLimits(ctx context.Context, providerID string) ([]workspace.FrontendAccount, error) {
	g.check("RefreshAccountLimits")
	return g.Workspace.RefreshAccountLimits(ctx, providerID)
}

func (g *updateGoroutineGuard) RefreshDockerMCPAvailability(ctx context.Context) (bool, error) {
	g.check("RefreshDockerMCPAvailability")
	return g.Workspace.RefreshDockerMCPAvailability(ctx)
}

func (g *updateGoroutineGuard) RefreshMCPTools(ctx context.Context, name string) error {
	g.check("RefreshMCPTools")
	return g.Workspace.RefreshMCPTools(ctx, name)
}

func (g *updateGoroutineGuard) RefreshOAuthToken(ctx context.Context, scope config.Scope, providerID string) error {
	g.check("RefreshOAuthToken")
	return g.Workspace.RefreshOAuthToken(ctx, scope, providerID)
}

func (g *updateGoroutineGuard) RefreshOAuthTokenForAccount(ctx context.Context, scope config.Scope, providerID string, accountID string) error {
	g.check("RefreshOAuthTokenForAccount")
	return g.Workspace.RefreshOAuthTokenForAccount(ctx, scope, providerID, accountID)
}

func (g *updateGoroutineGuard) RefreshProviderModels(ctx context.Context, providerID string) ([]workspace.ModelRefreshResult, error) {
	g.check("RefreshProviderModels")
	return g.Workspace.RefreshProviderModels(ctx, providerID)
}

func (g *updateGoroutineGuard) RemoveAccount(scope config.Scope, providerID string, accountID string) error {
	g.check("RemoveAccount")
	return g.Workspace.RemoveAccount(scope, providerID, accountID)
}

func (g *updateGoroutineGuard) RemoveConfigField(scope config.Scope, key string) error {
	g.check("RemoveConfigField")
	return g.Workspace.RemoveConfigField(scope, key)
}

func (g *updateGoroutineGuard) RemoveThread(ctx context.Context, id string, opts proto.RemoveThreadOptions) error {
	g.check("RemoveThread")
	return g.Workspace.RemoveThread(ctx, id, opts)
}

func (g *updateGoroutineGuard) RenameSession(ctx context.Context, sessionID string, title string) error {
	g.check("RenameSession")
	return g.Workspace.RenameSession(ctx, sessionID, title)
}

func (g *updateGoroutineGuard) SessionDescendantCost(ctx context.Context, sessionID string) (float64, error) {
	g.check("SessionDescendantCost")
	return g.Workspace.SessionDescendantCost(ctx, sessionID)
}

func (g *updateGoroutineGuard) SetConfigField(scope config.Scope, key string, value any) error {
	g.check("SetConfigField")
	return g.Workspace.SetConfigField(scope, key, value)
}

func (g *updateGoroutineGuard) SetCurrentSession(ctx context.Context, sessionID string) error {
	g.check("SetCurrentSession")
	return g.Workspace.SetCurrentSession(ctx, sessionID)
}

func (g *updateGoroutineGuard) SetCurrentSessionGeneration(ctx context.Context, sessionID string, generation uint64) error {
	g.check("SetCurrentSessionGeneration")
	return g.Workspace.SetCurrentSessionGeneration(ctx, sessionID, generation)
}

func (g *updateGoroutineGuard) SetProviderAPIKey(scope config.Scope, providerID string, apiKey string) error {
	g.check("SetProviderAPIKey")
	return g.Workspace.SetProviderAPIKey(scope, providerID, apiKey)
}

func (g *updateGoroutineGuard) SetProviderProxy(providerID string, proxy string) error {
	g.check("SetProviderProxy")
	return g.Workspace.SetProviderProxy(providerID, proxy)
}

func (g *updateGoroutineGuard) SkillStates() []*skills.SkillState {
	g.check("SkillStates")
	return g.Workspace.SkillStates()
}

func (g *updateGoroutineGuard) StartOAuth(ctx context.Context, providerID string, proxyURL string, forceNewAccount bool) (workspace.OAuthStartResult, workspace.OAuthFlow, error) {
	g.check("StartOAuth")
	return g.Workspace.StartOAuth(ctx, providerID, proxyURL, forceNewAccount)
}

func (g *updateGoroutineGuard) Stats(ctx context.Context, req stats.Request) (stats.Snapshot, error) {
	g.check("Stats")
	return g.Workspace.Stats(ctx, req)
}

func (g *updateGoroutineGuard) Subscribe(send func(any)) {
	g.check("Subscribe")
	g.Workspace.Subscribe(send)
}

func (g *updateGoroutineGuard) SubscribeWith(send func(any)) func() {
	g.check("SubscribeWith")
	return g.Workspace.SubscribeWith(send)
}

func (g *updateGoroutineGuard) UncommittedFiles(ctx context.Context) ([]git.FileChange, error) {
	g.check("UncommittedFiles")
	return g.Workspace.UncommittedFiles(ctx)
}

func (g *updateGoroutineGuard) UpdateAccountFields(providerID, accountID string, edit workspace.AccountEdit) error {
	g.check("UpdateAccountFields")
	return g.Workspace.UpdateAccountFields(providerID, accountID, edit)
}

func (g *updateGoroutineGuard) UpdateAgentModel(ctx context.Context) error {
	g.check("UpdateAgentModel")
	return g.Workspace.UpdateAgentModel(ctx)
}

func (g *updateGoroutineGuard) UpdatePreferredModel(scope config.Scope, model config.SelectedModel) error {
	g.check("UpdatePreferredModel")
	return g.Workspace.UpdatePreferredModel(scope, model)
}

func (g *updateGoroutineGuard) VerifyProviderAPIKey(ctx context.Context, providerID string, apiKey string) error {
	g.check("VerifyProviderAPIKey")
	return g.Workspace.VerifyProviderAPIKey(ctx, providerID, apiKey)
}

func (g *updateGoroutineGuard) WaitForMCPInit(ctx context.Context) error {
	g.check("WaitForMCPInit")
	return g.Workspace.WaitForMCPInit(ctx)
}
