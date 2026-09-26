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
	"os"
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
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/stats"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
)

// wireEnvVar is the CI "wire" job's switch (CLIENT-SERVER.md, "PR 0.7"):
// when set, this package's guarded harnesses (newCmdDrivenUI,
// newCmdDrivenGoldenUI, newBusyUI, newTestRoot) route every Workspace call
// through wsrpc.NewLoopback's JSON codec instead of calling the stub
// directly, so a type or error that would not survive a real wire hop
// breaks here in CI instead of only once gRPC exists.
const wireEnvVar = "SENNIT_TEST_WIRE"

// maybeWireWorkspace wraps ws in wsrpc.NewLoopback when wireEnvVar is set to
// "1", and returns ws unchanged otherwise.
//
// Ordering: every harness that also applies newUpdateGoroutineGuard wraps
// the *result* of maybeWireWorkspace in the guard, i.e. the guard sits
// outermost and the loopback innermost (UI -> guard -> loopback -> stub).
// Putting it the other way round would still let both layers see every
// call -- neither swallows one -- but it would interleave the loopback's
// own marshal/unmarshal frames between the UI's call site and g.check's
// captured stack, which check truncates to 800 bytes; with the guard
// outermost, check's stack starts at the actual offending caller instead
// of losing it to codec noise. Keep new guarded harnesses consistent with
// this order.
func maybeWireWorkspace(ws workspace.Workspace) workspace.Workspace {
	if os.Getenv(wireEnvVar) == "1" {
		return wsrpc.NewLoopback(ws)
	}
	return ws
}

// updateGoroutineGuardedMethods is every U/S/H method in
// wsrpc.MethodClasses (everything that is not C or X), computed here
// instead of hand-maintained as a duplicate list: this package can import
// wsrpc (a production package), unlike internal/workspace's own _test.go
// table it used to mirror by hand (recall commit 59666e391, which forbade
// putting a test-only table in a production file just to make it
// importable both ways — wsrpc's classes.go is that production file now).
//
// This only guarantees the *set* of guarded names stays in sync with
// wsrpc.MethodClasses; it says nothing about whether a wrapper method
// actually exists below for each one, and calling check() with a name
// missing an entry here would be a bug in the generated-looking block
// below, not in this table. internal/workspace/wire_classes_ui_guard_test.go
// (TestUIGuardHasWrapperForEveryWireMethod) reads this file's source as
// text and checks that every name here has a matching wrapper method.
var updateGoroutineGuardedMethods = func() map[string]bool {
	out := make(map[string]bool, len(wsrpc.MethodClasses))
	for name, class := range wsrpc.MethodClasses {
		if class == wsrpc.C || class == wsrpc.X {
			continue
		}
		out[name] = true
	}
	return out
}()

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
// all: PrepareSessionChanges used to be one of these (optional
// workspace.SessionChangePreparer, resolved by root.go's own type
// assertion) until PR 0.7c's review folded it into Workspace itself
// (FileServices) as a real, always-guaranteed member -- it is a plain
// generated-style U method below now, like any other. PrepareSessionChanges
// is what emptied this map; a future optional capability resolved the same
// way root.go used to would go back in here.
var extraGuardedMethods = map[string]bool{}

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

func (g *updateGoroutineGuard) PrepareSessionChanges(ctx context.Context, sessionID string) ([]workspace.SessionFile, error) {
	g.check("PrepareSessionChanges")
	return g.Workspace.PrepareSessionChanges(ctx, sessionID)
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

func (g *updateGoroutineGuard) ListProjectFiles(ctx context.Context, depth, limit int) ([]string, error) {
	g.check("ListProjectFiles")
	return g.Workspace.ListProjectFiles(ctx, depth, limit)
}

func (g *updateGoroutineGuard) AttachProjectFile(ctx context.Context, sessionID, path string) (message.Attachment, bool, error) {
	g.check("AttachProjectFile")
	return g.Workspace.AttachProjectFile(ctx, sessionID, path)
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

func (g *updateGoroutineGuard) ImportCopilot(ctx context.Context) (bool, error) {
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

func (g *updateGoroutineGuard) PendingPrompts(ctx context.Context) (workspace.PendingPrompts, error) {
	g.check("PendingPrompts")
	return g.Workspace.PendingPrompts(ctx)
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

func (g *updateGoroutineGuard) RecordAccount(scope config.Scope, providerID string, cred workspace.AccountCredential) (workspace.FrontendAccount, error) {
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
