package model

import (
	"context"
	"reflect"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	mcptools "github.com/rave-soft/braid/internal/agent/tools/mcp"
	"github.com/rave-soft/braid/internal/commands"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/git"
	"github.com/rave-soft/braid/internal/history"
	"github.com/rave-soft/braid/internal/lsp"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/oauth"
	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/question"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/skills"
	"github.com/rave-soft/braid/internal/ui/attachments"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/dialog"
	"github.com/rave-soft/braid/internal/workspace"
	"github.com/stretchr/testify/require"
)

// cmdDrivingWorkspace is a minimal workspace.Workspace stub that records
// calls for assertion in command-driving tests. It embeds workspace.Workspace
// so only explicitly overridden methods need implementation.
type cmdDrivingWorkspace struct {
	workspace.Workspace

	// Config stubs
	yolo       bool
	agentReady bool
	agentBusy  bool
	agentErr   error
	agentModel workspace.AgentModel

	// Call counters
	createSessionCalls    int
	agentRunCalls         int
	agentRunSession       string
	agentRunPrompt        string
	agentBusyCalls        int
	agentReadyCalls       int
	agentReadyErrCalls    int
	agentModelCalls       int
	agentQueuedCalls      int
	agentClearQueueCalls  int
	agentCancelCalls      int
	agentSummarizeCalls   int
	agentUpdateModelCalls int
	agentRunShellCalls    int

	permGrantCalls          int
	permGrantPersistentCall permission.PermissionRequest
	permDenyCalls           int
	permSkipCalls           int

	listMessagesCalls int
	listThreadsCalls  int
	getSessionCalls   int

	// Session return value
	returnSession session.Session
}

func (w *cmdDrivingWorkspace) Config() *config.Config {
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("test-provider", config.ProviderConfig{ID: "test-provider"})
	return &config.Config{
		Providers: providers,
		Options:   &config.Options{TUI: &config.TUIOptions{}},
	}
}

func (w *cmdDrivingWorkspace) WorkingDir() string                { return "/tmp" }
func (w *cmdDrivingWorkspace) Resolver() config.VariableResolver { return nil }
func (w *cmdDrivingWorkspace) UncommittedFiles(context.Context) ([]git.FileChange, error) {
	return nil, nil
}

func (w *cmdDrivingWorkspace) PermissionSkipRequests() bool {
	w.permSkipCalls++
	return w.yolo
}
func (w *cmdDrivingWorkspace) PermissionSetSkipRequests(bool) {}
func (w *cmdDrivingWorkspace) PermissionGrant(p permission.PermissionRequest) bool {
	w.permGrantCalls++
	return true
}

func (w *cmdDrivingWorkspace) PermissionGrantPersistent(p permission.PermissionRequest) bool {
	w.permGrantPersistentCall = p
	w.permGrantCalls++
	return true
}

func (w *cmdDrivingWorkspace) PermissionDeny(p permission.PermissionRequest) bool {
	w.permDenyCalls++
	return true
}

func (w *cmdDrivingWorkspace) AgentIsReady() bool {
	w.agentReadyCalls++
	return w.agentReady
}

func (w *cmdDrivingWorkspace) AgentReadyErr() error {
	w.agentReadyErrCalls++
	return w.agentErr
}

func (w *cmdDrivingWorkspace) AgentIsBusy() bool {
	w.agentBusyCalls++
	return w.agentBusy
}

func (w *cmdDrivingWorkspace) AgentModel() workspace.AgentModel {
	w.agentModelCalls++
	return w.agentModel
}

func (w *cmdDrivingWorkspace) AgentQueuedPrompts(string) int { w.agentQueuedCalls++; return 0 }

func (w *cmdDrivingWorkspace) AgentQueuedPromptsList(string) []string {
	w.agentQueuedCalls++
	return nil
}
func (w *cmdDrivingWorkspace) AgentClearQueue(string) { w.agentClearQueueCalls++ }
func (w *cmdDrivingWorkspace) AgentCancel(string)     { w.agentCancelCalls++ }
func (w *cmdDrivingWorkspace) AgentSummarize(ctx context.Context, s string) error {
	w.agentSummarizeCalls++
	return nil
}

func (w *cmdDrivingWorkspace) UpdateAgentModel(ctx context.Context) error {
	w.agentUpdateModelCalls++
	return nil
}

func (w *cmdDrivingWorkspace) AgentRun(
	_ context.Context, sessionID, prompt string, _ ...message.Attachment,
) error {
	w.agentRunCalls++
	w.agentRunSession = sessionID
	w.agentRunPrompt = prompt
	return nil
}

func (w *cmdDrivingWorkspace) AgentRunShellCommand(
	ctx context.Context, sessionID, command string, termWidth int,
	onProgress func(string), isFirstMessage bool,
) (proto.ShellCommandResponse, error) {
	w.agentRunShellCalls++
	return proto.ShellCommandResponse{}, nil
}

func (w *cmdDrivingWorkspace) AgentRunStream(ctx context.Context, s, p string) (<-chan workspace.AgentRunEvent, error) {
	return nil, nil
}
func (w *cmdDrivingWorkspace) AgentIsSessionBusy(string) bool { return false }

func (w *cmdDrivingWorkspace) CreateSession(_ context.Context, title string) (session.Session, error) {
	w.createSessionCalls++
	s := w.returnSession
	if s.ID == "" {
		s = session.Session{ID: "sess-" + title}
	}
	return s, nil
}

func (w *cmdDrivingWorkspace) GetSession(_ context.Context, id string) (session.Session, error) {
	w.getSessionCalls++
	return session.Session{ID: id}, nil
}

func (w *cmdDrivingWorkspace) ListMessages(_ context.Context, id string) ([]message.Message, error) {
	w.listMessagesCalls++
	return nil, nil
}

func (w *cmdDrivingWorkspace) ListUserMessages(_ context.Context, _ string) ([]message.Message, error) {
	return nil, nil
}

func (w *cmdDrivingWorkspace) ListAllUserMessages(_ context.Context) ([]message.Message, error) {
	return nil, nil
}

func (w *cmdDrivingWorkspace) SaveSession(_ context.Context, s session.Session) (session.Session, error) {
	return s, nil
}

func (w *cmdDrivingWorkspace) DeleteSession(_ context.Context, _ string) error {
	return nil
}
func (w *cmdDrivingWorkspace) CreateAgentToolSessionID(_, _ string) string { return "" }
func (w *cmdDrivingWorkspace) ParseAgentToolSessionID(_ string) (string, string, bool) {
	return "", "", false
}
func (w *cmdDrivingWorkspace) SetCurrentSession(_ context.Context, _ string) error { return nil }

func (w *cmdDrivingWorkspace) SupportsThreads() bool { return false }
func (w *cmdDrivingWorkspace) ListThreads(_ context.Context) ([]proto.Thread, error) {
	w.listThreadsCalls++
	return nil, nil
}

func (w *cmdDrivingWorkspace) GetThread(_ context.Context, _ string) (proto.Thread, error) {
	return proto.Thread{}, nil
}

func (w *cmdDrivingWorkspace) CreateThread(_ context.Context, _ proto.CreateThreadRequest) (proto.Thread, error) {
	return proto.Thread{}, nil
}
func (w *cmdDrivingWorkspace) SendThread(_ context.Context, _, _ string) error { return nil }
func (w *cmdDrivingWorkspace) MergeThread(_ context.Context, _ string) (proto.Thread, error) {
	return proto.Thread{}, nil
}

func (w *cmdDrivingWorkspace) RemoveThread(_ context.Context, _ string, _ proto.RemoveThreadOptions) error {
	return nil
}

func (w *cmdDrivingWorkspace) AttachThread(_ context.Context, _ string) (workspace.Workspace, func(), error) {
	return nil, func() {}, nil
}

func (w *cmdDrivingWorkspace) InitCoderAgent(ctx context.Context) error { return nil }
func (w *cmdDrivingWorkspace) InitCoderAgentNonInteractive(ctx context.Context) error {
	return nil
}

func (w *cmdDrivingWorkspace) GetDefaultSmallModel(string) config.SelectedModel {
	return config.SelectedModel{}
}
func (w *cmdDrivingWorkspace) LSPStart(ctx context.Context, path string) {}
func (w *cmdDrivingWorkspace) LSPStopAll(ctx context.Context)            {}
func (w *cmdDrivingWorkspace) LSPGetStates() map[string]workspace.LSPClientInfo {
	return nil
}

func (w *cmdDrivingWorkspace) LSPGetDiagnosticCounts(name string) lsp.DiagnosticCounts {
	return lsp.DiagnosticCounts{}
}

func (w *cmdDrivingWorkspace) UpdatePreferredModel(config.Scope, config.SelectedModel) error {
	return nil
}

func (w *cmdDrivingWorkspace) OverridePreferredModel(config.SelectedModel) error {
	return nil
}

func (w *cmdDrivingWorkspace) SetCompactMode(config.Scope, bool) error {
	return nil
}

func (w *cmdDrivingWorkspace) SetProviderAPIKey(config.Scope, string, any) error {
	return nil
}

func (w *cmdDrivingWorkspace) SetConfigField(config.Scope, string, any) error {
	return nil
}

func (w *cmdDrivingWorkspace) RemoveConfigField(config.Scope, string) error {
	return nil
}

func (w *cmdDrivingWorkspace) ImportCopilot() (*oauth.Token, bool) {
	return nil, false
}

func (w *cmdDrivingWorkspace) RefreshOAuthToken(ctx context.Context, scope config.Scope, providerID string) error {
	return nil
}

func (w *cmdDrivingWorkspace) ProjectNeedsInitialization() (bool, error) {
	return false, nil
}
func (w *cmdDrivingWorkspace) MarkProjectInitialized() error { return nil }
func (w *cmdDrivingWorkspace) InitializePrompt() (string, error) {
	return "", nil
}

func (w *cmdDrivingWorkspace) ListSkills(ctx context.Context) ([]skills.CatalogEntry, error) {
	return nil, nil
}

func (w *cmdDrivingWorkspace) ReadSkill(ctx context.Context, skillID string) ([]byte, skills.SkillReadResult, error) {
	return nil, skills.SkillReadResult{}, nil
}
func (w *cmdDrivingWorkspace) WaitForMCPInit(ctx context.Context) error { return nil }
func (w *cmdDrivingWorkspace) MCPGetStates() map[string]mcptools.ClientInfo {
	return nil
}

func (w *cmdDrivingWorkspace) MCPResources() []workspace.MCPResourceInfo {
	return nil
}
func (w *cmdDrivingWorkspace) MCPRefreshPrompts(ctx context.Context, name string)   {}
func (w *cmdDrivingWorkspace) MCPRefreshResources(ctx context.Context, name string) {}
func (w *cmdDrivingWorkspace) RefreshMCPTools(ctx context.Context, name string)     {}
func (w *cmdDrivingWorkspace) ReadMCPResource(ctx context.Context, name, uri string) ([]workspace.MCPResourceContents, error) {
	return nil, nil
}

func (w *cmdDrivingWorkspace) ListMCPPrompts(ctx context.Context) ([]commands.MCPPrompt, error) {
	return nil, nil
}

func (w *cmdDrivingWorkspace) GetMCPPrompt(string, string, map[string]string) (string, error) {
	return "", nil
}

func (w *cmdDrivingWorkspace) EnableDockerMCP(ctx context.Context) error {
	return nil
}
func (w *cmdDrivingWorkspace) DisableDockerMCP() error { return nil }
func (w *cmdDrivingWorkspace) MCPAuthenticate(ctx context.Context, name string) error {
	return nil
}

func (w *cmdDrivingWorkspace) MCPPendingAuth() []mcptools.PendingAuthServer {
	return nil
}

func (w *cmdDrivingWorkspace) MCPAuthURL(string) string                                          { return "" }
func (w *cmdDrivingWorkspace) FileTrackerRecordRead(ctx context.Context, sessionID, path string) {}
func (w *cmdDrivingWorkspace) FileTrackerLastReadTime(ctx context.Context, sessionID, path string) time.Time {
	return time.Time{}
}

func (w *cmdDrivingWorkspace) FileTrackerListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	return nil, nil
}

func (w *cmdDrivingWorkspace) ListSessionHistory(ctx context.Context, sessionID string) ([]history.File, error) {
	return nil, nil
}
func (w *cmdDrivingWorkspace) QuestionAnswer(responses []question.Answer) bool { return false }
func (w *cmdDrivingWorkspace) QuestionCancel() bool                            { return false }
func (w *cmdDrivingWorkspace) Subscribe(*tea.Program)                          {}
func (w *cmdDrivingWorkspace) Shutdown()                                       {}

// ---------------------------------------------------------------------------
// cmdDrivenUI builds a UI over cmdDrivingWorkspace with all caches warm.
// ---------------------------------------------------------------------------

func newCmdDrivenUI(ws *cmdDrivingWorkspace) *UI {
	com := common.DefaultCommon(context.Background(), ws)
	return &UI{
		com:    com,
		status: NewStatus(com, nil),
		chat:   NewChat(com, config.ScrollbarDefault),
		editor: editorState{
			textarea:    textarea.New(),
			attachments: attachments.New(nil, attachments.Keymap{}),
		},
		state:   uiChat,
		focus:   uiFocusEditor,
		width:   140,
		height:  45,
		session: &session.Session{ID: "s1"},
		keyMap:  DefaultKeyMap(),
		dialog:  dialog.NewOverlay(),
	}
}

// warmCmdDrivenCaches marks all memoized workspace state fresh so only
// explicit state transitions (not startup staleness) trigger refresh dispatches.
func warmCmdDrivenCaches(m *UI) {
	m.wsCache.agentBusyCache.set(false)
	m.wsCache.yoloCache.set(false)
	m.wsCache.agentReady = true
	m.wsCache.promptQueueCheckedAt = time.Now()
	m.lspCheckedAt = time.Now()
}

// sequenceMsgType is the exact unexported wrapper type returned by
// tea.Sequence. It must be identified by type rather than by slice shape:
// tea.EnvMsg and arbitrary named slices are ordinary messages, not commands.
var sequenceMsgType = reflect.TypeOf(tea.Sequence(
	func() tea.Msg { return nil },
	func() tea.Msg { return nil },
)())

// isCommandSliceWrapper recognizes only Bubble Tea's two command wrappers.
func isCommandSliceWrapper(msg tea.Msg) ([]tea.Cmd, bool) {
	if batch, ok := msg.(tea.BatchMsg); ok {
		return batch, true
	}
	if reflect.TypeOf(msg) != sequenceMsgType {
		return nil, false
	}

	sequence := reflect.ValueOf(msg)
	cmds := make([]tea.Cmd, 0, sequence.Len())
	for i := range sequence.Len() {
		cmds = append(cmds, sequence.Index(i).Interface().(tea.Cmd))
	}
	return cmds, true
}

// driveCmdStep executes one leaf-producing command and immediately routes its
// message through Update. It deliberately does not execute Update's returned
// command, allowing tests to inspect state at asynchronous boundaries.
func driveCmdStep(m *UI, cmd tea.Cmd) (tea.Msg, tea.Cmd) {
	msg := cmd()
	_, next := m.Update(msg)
	return msg, next
}

// runCmdTree executes a tea.Cmd like the Bubble Tea runtime. Every leaf,
// including unknown messages, is routed through Update. afterUpdate runs
// before any command returned by Update, so callers can assert intermediate
// state; it may be nil.
func runCmdTree(m *UI, cmd tea.Cmd, afterUpdate func(tea.Msg, tea.Cmd)) []tea.Msg {
	if cmd == nil {
		return nil
	}

	var messages []tea.Msg
	stack := []tea.Cmd{cmd}
	for len(stack) > 0 {
		cmd := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		msg := cmd()
		if cmds, ok := isCommandSliceWrapper(msg); ok {
			for i := len(cmds) - 1; i >= 0; i-- {
				stack = append(stack, cmds[i])
			}
			continue
		}

		_, next := m.Update(msg)
		messages = append(messages, msg)
		if afterUpdate != nil {
			afterUpdate(msg, next)
		}
		if next != nil {
			stack = append(stack, next)
		}
	}
	return messages
}

// ---------------------------------------------------------------------------
// Test: Command execution — a dispatched tea.Cmd fires and its result
// lands as a message in the Update loop.
// ---------------------------------------------------------------------------

func TestCmdDriving_CommandExecution_DispatchAndApply(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true, agentBusy: true}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	// Directly dispatch a busy refresh (simulating what staleWorkspaceRefreshCmds does).
	cmd := m.dispatchBusyRefresh()
	require.NotNil(t, cmd, "dispatchBusyRefresh should produce a cmd")

	messages := runCmdTree(m, cmd, nil)

	// We should have received a busyStateMsg.
	var gotBusy bool
	for _, msg := range messages {
		if _, ok := msg.(busyStateMsg); ok {
			gotBusy = true
		}
	}
	require.True(t, gotBusy, "command-driving must deliver a busyStateMsg")
}

// ---------------------------------------------------------------------------
// Test: Stale-result guard — a fetch started before a newer state transition
// must be discarded and re-dispatched, then re-run through runCmdTree.
// ---------------------------------------------------------------------------

func TestCmdDriving_StaleResultGuard_DiscardedAndRedispatched(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true, agentBusy: true}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	// Optimistically set busy=true (simulates sendMessage's optimistic
	// write), then bump the generation to simulate a state transition.
	m.wsCache.agentBusyCache.set(true)
	m.wsCache.busyFetchGen++

	// Simulate the stale probe arriving by dispatching it through a cmd
	// so the full chain (cmd → Update → applyBusyState → stale check) runs.
	staleGen := m.wsCache.busyFetchGen - 1
	staleCmd := func() tea.Msg {
		return busyStateMsg{gen: staleGen, agentBusy: false, ready: true}
	}

	// Deliver exactly the stale command, then inspect state before executing
	// the authoritative refresh returned by Update.
	msg, freshCmd := driveCmdStep(m, staleCmd)
	require.IsType(t, busyStateMsg{}, msg)
	require.NotNil(t, freshCmd, "a stale result must schedule an authoritative refresh")
	require.True(t, m.isAgentBusy(),
		"stale result must not overwrite optimistic busy state before refresh runs")
	require.True(t, m.wsCache.busyFetchInFlight,
		"the stale result must leave the replacement probe in flight")

	// Now drive the separately returned current-generation probe.
	messages := runCmdTree(m, freshCmd, nil)
	require.Contains(t, messages, busyStateMsg{gen: m.wsCache.busyFetchGen, ready: true, agentBusy: true},
		"the replacement command must execute and deliver its result")
}

// ---------------------------------------------------------------------------
// Test: Repeated Enter — message send via UI.Update → busy state optimistic
// update → re-fetch.
// ---------------------------------------------------------------------------

func TestCmdDriving_RepeatedEnter_SendAndSubmit(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{
		agentReady:    true,
		agentErr:      nil,
		returnSession: session.Session{ID: "new-sess"},
	}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)
	m.editor.textarea.SetValue("hello")

	// Simulate pressing Enter: goes through UI.Update → handleKeyPressMsg
	// → matches Editor.SendMessage → tea.Batch(sendMessage, loadPromptHistory).
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd, "Enter key must produce a cmd")

	messages := runCmdTree(m, cmd, nil)

	// The cmd chain should fire AgentRun (side effect) and produce
	// agentRunSubmittedMsg which Update turns into a busy refresh.
	var gotRunSubmitted bool
	for _, msg := range messages {
		if _, ok := msg.(agentRunSubmittedMsg); ok {
			gotRunSubmitted = true
		}
	}
	require.True(t, gotRunSubmitted, "Enter must produce agentRunSubmittedMsg")

	// Verify the workspace call fired.
	require.Equal(t, 1, ws.agentRunCalls, "AgentRun must be called with the prompt")
	require.Equal(t, "s1", ws.agentRunSession, "AgentRun must target the current session")
	require.Equal(t, "hello", ws.agentRunPrompt, "AgentRun must carry the message content")

	// After agentRunSubmittedMsg, the model re-fetches busy state.
	require.Equal(t, 1, ws.agentBusyCalls, "busy state must be re-probed after send")
}

// ---------------------------------------------------------------------------
// Test: Permission round-trip — open permissions dialog, action via
// handleDialogMsg (real Update path), workspace call.
// ---------------------------------------------------------------------------

func TestCmdDriving_PermissionRoundTrip_Allow(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	perm := permission.PermissionRequest{
		ID:         "perm-1",
		ToolCallID: "tool-call-X",
		ToolName:   "bash",
	}

	// Open the permissions dialog (use OpenDialog to skip the grace period
	// that would absorb the key press in tests).
	permsDialog := dialog.NewPermissions(m.com, perm)
	m.dialog.OpenDialog(permsDialog)

	require.True(t, m.dialog.ContainsDialog(dialog.PermissionsID),
		"permissions dialog must be open")

	// Simulate pressing 'a' (Allow) through the real Update path:
	// UI.Update → tea.KeyPressMsg → handleKeyPressMsg →
	// handleDialogMsg → permsDialog.HandleMsg → ActionPermissionResponse →
	// applyDialogAction (side effects: PermissionGrant + CloseDialog).
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	// The permission action is all side-effects (PermissionGrant + CloseDialog)
	// and produces no tea.Cmd; the cmd is nil here.
	require.Nil(t, cmd, "allow action produces no cmd — only side effects")

	// Verify workspace call fired.
	require.GreaterOrEqual(t, ws.permGrantCalls, 1,
		"workspace.PermissionGrant must be called after Allow action")

	// Dialog must close.
	require.False(t, m.dialog.ContainsDialog(dialog.PermissionsID),
		"permissions dialog must close after action")

	// Verify the action flowed through — applyDialogAction for
	// ActionPermissionResponse is side-effects only (no cmds), so we
	// verify via workspace state.
	require.GreaterOrEqual(t, ws.permGrantCalls, 1)
}

// ---------------------------------------------------------------------------
// Test: Routing — message types route to correct handlers.
// Verify that distinct tea.Msg types hit different case branches in Update.
// ---------------------------------------------------------------------------

func TestCmdDriving_Routing_DistinctMsgTypes(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	// 1. busyStateMsg → applyBusyState path. Start from the opposite
	// readiness value so this proves the routing branch changed state.
	m.wsCache.agentReady = false
	_, cmd := m.Update(busyStateMsg{gen: m.wsCache.busyFetchGen, ready: true, agentBusy: false})
	_ = runCmdTree(m, cmd, nil)
	require.True(t, m.wsCache.agentReady, "busyStateMsg must route to applyBusyState")

	// 2. promptQueueMsg → applyPromptQueue path.
	_, cmd = m.Update(promptQueueMsg{forSession: "s1", gen: m.wsCache.promptQueueGen, prompts: []string{"p1"}})
	_ = runCmdTree(m, cmd, nil)
	require.Equal(t, 1, m.wsCache.promptQueue, "promptQueueMsg must update queue")

	// 3. agentRunSubmittedMsg → invalidation + re-dispatch path.
	m.wsCache.agentBusyCache.set(true)
	_, cmd = m.Update(agentRunSubmittedMsg{})
	_ = runCmdTree(m, cmd, nil)
	require.False(t, m.isAgentBusy(),
		"agentRunSubmittedMsg must trigger re-fetch of authoritative state")

	// 4. sendMessageMsg → sendMessage path.
	_, cmd = m.Update(sendMessageMsg{Content: "routed-msg"})
	_ = runCmdTree(m, cmd, nil)
	require.Equal(t, 1, ws.agentRunCalls, "sendMessageMsg must route to sendMessage")
}

// ---------------------------------------------------------------------------
// Test: Stale prompt queue — result from wrong session must be discarded.
// Goes through runCmdTree so applyPromptQueue is exercised via Update.
// ---------------------------------------------------------------------------

func TestCmdDriving_StalePromptQueue_WrongSession(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	// The current session is "s1". A stale fetch from "s2" arrives via
	// runCmdTree so the full Update → applyPromptQueue chain runs.
	staleGen := m.wsCache.promptQueueGen
	staleCmd := func() tea.Msg {
		return promptQueueMsg{
			forSession: "s2",
			gen:        staleGen,
			prompts:    []string{"stale"},
		}
	}

	_, freshCmd := driveCmdStep(m, staleCmd)
	require.Zero(t, m.wsCache.promptQueue,
		"result from different session must not populate queue before refresh runs")
	require.NotNil(t, freshCmd, "session-mismatched result must schedule a refresh")
	require.True(t, m.wsCache.promptQueueInFlight,
		"session-mismatched result must leave the replacement fetch in flight")

	// Execute the replacement separately and verify the command performs its
	// workspace probe.
	_ = runCmdTree(m, freshCmd, nil)
	require.Positive(t, ws.agentReadyCalls,
		"the replacement queue-refresh command must execute")
}

// ---------------------------------------------------------------------------
// Test: Enter when no session — creates session, sends message.
// Goes through UI.Update → handleKeyPressMsg → sendMessage path.
// ---------------------------------------------------------------------------

func TestCmdDriving_EnterWhenNoSession_CreatesSession(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{
		agentReady:    true,
		agentErr:      nil,
		returnSession: session.Session{ID: "auto-created"},
	}
	// No session set — m.session is nil.
	m := newCmdDrivenUI(ws)
	m.session = nil
	warmCmdDrivenCaches(m)
	m.editor.textarea.SetValue("first message")

	// Simulate pressing Enter: goes through UI.Update → handleKeyPressMsg
	// → matches Editor.SendMessage → tea.Batch(sendMessage, loadPromptHistory).
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd, "Enter with no session must produce a cmd")

	messages := runCmdTree(m, cmd, nil)

	// Must have created a session.
	require.GreaterOrEqual(t, ws.createSessionCalls, 1,
		"sendMessage with no session must create one")

	// Must have sent the message (AgentRun).
	require.GreaterOrEqual(t, ws.agentRunCalls, 1,
		"after session creation, message must be sent")

	// Must have received a loadSessionMsg for the new session.
	var gotLoadSession bool
	for _, msg := range messages {
		if _, ok := msg.(loadSessionMsg); ok {
			gotLoadSession = true
		}
	}
	require.True(t, gotLoadSession, "session creation must trigger loadSessionMsg")
}

// ---------------------------------------------------------------------------
// Test: Permission deny path — deny action reaches workspace.PermissionDeny.
// ---------------------------------------------------------------------------

func TestCmdDriving_PermissionRoundTrip_Deny(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	perm := permission.PermissionRequest{
		ID:         "perm-2",
		ToolCallID: "tool-call-Y",
		ToolName:   "bash",
	}

	// Open the permissions dialog (use OpenDialog to skip the grace period
	// that would absorb the key press in tests).
	permsDialog := dialog.NewPermissions(m.com, perm)
	m.dialog.OpenDialog(permsDialog)

	// Simulate pressing 'd' (Deny) through the real Update path:
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	// The permission action is side-effects only.
	require.Nil(t, cmd, "deny action produces no cmd — only side effects")

	// Verify workspace call fired.
	require.GreaterOrEqual(t, ws.permDenyCalls, 1,
		"workspace.PermissionDeny must be called after Deny action")

	// Dialog must close.
	require.False(t, m.dialog.ContainsDialog(dialog.PermissionsID),
		"permissions dialog must close after Deny action")
}

// ---------------------------------------------------------------------------
// Test: Allow-for-session round-trip — PermissionGrantPersistent is called.
// ---------------------------------------------------------------------------

func TestCmdDriving_PermissionRoundTrip_AllowForSession(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	perm := permission.PermissionRequest{
		ID:         "perm-3",
		ToolCallID: "tool-call-Z",
		ToolName:   "write",
	}

	permsDialog := dialog.NewPermissions(m.com, perm)
	// Open the permissions dialog (use OpenDialog to skip the grace period).
	m.dialog.OpenDialog(permsDialog)

	// Press 's' (Allow for session) through the real Update path:
	_, cmd := m.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	// Side-effects only.
	require.Nil(t, cmd, "allow-for-session action produces no cmd — only side effects")

	// The workspace call must use the persistent grant path.
	require.GreaterOrEqual(t, ws.permGrantCalls, 1,
		"workspace.PermissionGrantPersistent must be called after Allow-for-Session")
	require.Equal(t, perm, ws.permGrantPersistentCall,
		"the persistent grant must carry the original permission request")

	// Dialog must close.
	require.False(t, m.dialog.ContainsDialog(dialog.PermissionsID),
		"permissions dialog must close after Allow-for-Session action")
}

// ---------------------------------------------------------------------------
// Test: Repeated Enter — second Enter after first completes.
// Verifies the UI handles two sequential sends without panicking.
// ---------------------------------------------------------------------------

func TestCmdDriving_RepeatedEnter_SequentialSends(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	// First send via UI.Update.
	m.editor.textarea.SetValue("first")
	_, cmd1 := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd1)
	_ = runCmdTree(m, cmd1, nil)
	require.Equal(t, 1, ws.agentRunCalls)

	// Second send — should not panic and should fire a second AgentRun.
	m.editor.textarea.SetValue("second")
	_, cmd2 := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd2)
	messages := runCmdTree(m, cmd2, nil)

	require.Equal(t, 2, ws.agentRunCalls, "two sends must produce two AgentRun calls")
	require.Equal(t, "second", ws.agentRunPrompt, "second send must carry correct content")

	// Must have received another agentRunSubmittedMsg.
	var runSubmittedCount int
	for _, msg := range messages {
		if _, ok := msg.(agentRunSubmittedMsg); ok {
			runSubmittedCount++
		}
	}
	require.Equal(t, 1, runSubmittedCount, "second send must produce agentRunSubmittedMsg")
}

// ---------------------------------------------------------------------------
// Test: tea.Sequence support — runCmdTree expands sequenceMsg via
// reflection and drives each sub-command in order.
// ---------------------------------------------------------------------------

func TestCmdDriving_SequenceSupport_DrivesInOrder(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	// Build a tea.Sequence(cmd1, cmd2) where each cmd produces a
	// busyStateMsg.  The harness must expand the sequence and drive both.
	var callOrder []string
	cmd1 := func() tea.Msg {
		callOrder = append(callOrder, "cmd1")
		return busyStateMsg{gen: m.wsCache.busyFetchGen, agentBusy: false}
	}
	cmd2 := func() tea.Msg {
		callOrder = append(callOrder, "cmd2")
		return busyStateMsg{gen: m.wsCache.busyFetchGen, agentBusy: true}
	}

	seq := tea.Sequence(cmd1, cmd2)
	messages := runCmdTree(m, seq, nil)

	require.Equal(t, []string{"cmd1", "cmd2"}, callOrder,
		"tea.Sequence sub-commands must be driven in order")
	require.Len(t, messages, 2,
		"both sequence messages must be delivered through Update")
	_, ok1 := messages[0].(busyStateMsg)
	_, ok2 := messages[1].(busyStateMsg)
	require.True(t, ok1 && ok2, "both messages must be busyStateMsg")
}

// ---------------------------------------------------------------------------
// Test: Unknown leaf messages flow through Update, not dropped.
// A tea.Cmd produces a custom unknown msg AND a slice-shaped msg;
// runCmdTree must deliver both through Update rather than dropping them.
// ---------------------------------------------------------------------------

type (
	unknownMsg     struct{ id string }
	sliceShapedMsg []string // named slice, but not a Bubble Tea command wrapper
)

func TestCmdDriving_UnknownMessage_RoutedThroughUpdate(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	// A cmd that returns an unknown message type — not matched by any
	// Update case, so it falls through the default handler.
	unknownCmd := func() tea.Msg { return unknownMsg{id: "test-1"} }

	// This named slice MUST NOT be treated as a command wrapper.
	sliceCmd := func() tea.Msg { return sliceShapedMsg{"a", "b"} }

	// The callback instruments Update itself, rather than merely proving that
	// leaves were collected. tea.EnvMsg has a known routing effect (it returns
	// a terminal query command); unknownMsg and the named slice do not.
	updated := make(map[reflect.Type]tea.Cmd)
	messages := runCmdTree(m, tea.Batch(unknownCmd, sliceCmd, func() tea.Msg {
		return tea.EnvMsg{"WT_SESSION=1"}
	}), func(msg tea.Msg, next tea.Cmd) {
		updated[reflect.TypeOf(msg)] = next
	})

	// EnvMsg's returned terminal query command may itself produce a response,
	// so assert the three original leaves by their Update instrumentation.
	require.GreaterOrEqual(t, len(messages), 3, "every non-wrapper leaf must be delivered")
	require.Contains(t, updated, reflect.TypeOf(unknownMsg{}), "unknown message must reach Update")
	require.Contains(t, updated, reflect.TypeOf(sliceShapedMsg{}), "named slice must reach Update")
	require.Nil(t, updated[reflect.TypeOf(unknownMsg{})], "unknown message follows Update's default route")
	require.Nil(t, updated[reflect.TypeOf(sliceShapedMsg{})], "named slice must not be expanded as commands")
	require.NotNil(t, updated[reflect.TypeOf(tea.EnvMsg{})], "tea.EnvMsg must run its Update route")
}
