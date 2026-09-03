package model

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	providerruntime "github.com/rave-soft/sennit/internal/providers/runtime"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/spin"
	"github.com/rave-soft/sennit/internal/stats"
	"github.com/rave-soft/sennit/internal/ui/attachments"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/chatlist"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	fimage "github.com/rave-soft/sennit/internal/ui/image"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
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
	createSessionErr      error
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

	applySessionModelCalls    int
	applySessionModelSwitched bool
	applySessionModelErr      error
	agentRunShellCalls        int

	permGrantCalls          int
	permGrantPersistentCall permission.PermissionRequest
	permDenyCalls           int
	permSkipCalls           int

	listMessagesCalls       int
	listMessagesByIDsCalls  int
	listMessagesByIDs       [][]string
	listMessagesErrByID     map[string]error
	listThreadsCalls        int
	supportsThreads         bool
	threads                 []proto.Thread
	getSessionCalls         int
	listSessionHistoryCalls int
	lspStartCalls           int
	setCurrentSessionCalls  int
	currentSessionIDs       []string

	questionAnswerCalls    int
	questionAnswerBatchID  string
	questionAnswerResponse []question.Answer
	questionCancelCalls    int

	sessionsBySessionID map[string]session.Session
	messagesBySessionID map[string][]message.Message
	historyBySessionID  map[string][]history.File

	// Account stubs
	listAccountsCalls  int
	accs               []accounts.Account
	listAccountsErr    error
	updateAccountCalls int
	lastUpdatedAccount accounts.Account
	updateAccountErr   error
	removeAccountCalls int
	lastRemovedID      string
	removeAccountErr   error

	setProviderProxyCalls int
	lastSetProviderProxy  string
	setProviderProxyErr   error
}

// KnownProviders mirrors what the UI used to compute for itself: the
// embedded catalog for this fake's config. BuiltinSkills likewise ships
// what the binary ships, since the skills panel renders it.
//
// ConfigProblems and SkillStates are empty: these tests drive commands,
// not the diagnostics behind them.
func (w cmdDrivingWorkspace) ConfigProblems() []config.Problem  { return nil }
func (w cmdDrivingWorkspace) SkillStates() []*skills.SkillState { return nil }
func (w cmdDrivingWorkspace) BuiltinSkills() []*skills.Skill    { return skills.DiscoverBuiltin() }
func (w *cmdDrivingWorkspace) KnownProviders() []catwalk.Provider {
	return providerruntime.Providers(w.Config().Options.DisableDefaultProviders)
}

// CurrentPlanUsage: no provider in these tests quotes rate limits, so the
// sidebar renders no plan line.
func (w *cmdDrivingWorkspace) CurrentPlanUsage(string) (accounts.Usage, bool) {
	return accounts.Usage{}, false
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
func (w *cmdDrivingWorkspace) BackgroundJobCounts() workspace.BackgroundJobCounts {
	return workspace.BackgroundJobCounts{}
}

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

// applySessionModelResult is what ApplySessionModel reports back: false is
// the common case (the session pins no model, or is already on it), so it
// is the default a test does not have to opt out of.
func (w *cmdDrivingWorkspace) ApplySessionModel(context.Context, string) (bool, error) {
	w.applySessionModelCalls++
	return w.applySessionModelSwitched, w.applySessionModelErr
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

func (w *cmdDrivingWorkspace) AgentRunStream(ctx context.Context, s, p string, opts workspace.AgentRunOptions) (<-chan workspace.AgentRunEvent, error) {
	return nil, nil
}
func (w *cmdDrivingWorkspace) AgentIsSessionBusy(string) bool { return false }

func (w *cmdDrivingWorkspace) CreateSession(_ context.Context, title string) (session.Session, error) {
	w.createSessionCalls++
	if w.createSessionErr != nil {
		return session.Session{}, w.createSessionErr
	}
	return session.Session{ID: "sess-" + title}, nil
}

func (w *cmdDrivingWorkspace) GetSession(_ context.Context, id string) (session.Session, error) {
	w.getSessionCalls++
	s, ok := w.sessionsBySessionID[id]
	if !ok {
		return session.Session{}, fmt.Errorf("session %s not found", id)
	}
	return s, nil
}

func (w *cmdDrivingWorkspace) ListMessages(_ context.Context, id string) ([]message.Message, error) {
	w.listMessagesCalls++
	if err := w.listMessagesErrByID[id]; err != nil {
		return nil, err
	}
	msgs, ok := w.messagesBySessionID[id]
	if !ok {
		return nil, nil
	}
	result := make([]message.Message, len(msgs))
	copy(result, msgs)
	return result, nil
}

func (w *cmdDrivingWorkspace) ListMessagesBySessionIDs(_ context.Context, _ string, _ uint64, ids []string) (map[string][]message.Message, error) {
	w.listMessagesByIDsCalls++
	w.listMessagesByIDs = append(w.listMessagesByIDs, append([]string(nil), ids...))
	result := make(map[string][]message.Message)
	for _, id := range ids {
		if err := w.listMessagesErrByID[id]; err != nil {
			return nil, err
		}
		msgs, ok := w.messagesBySessionID[id]
		if ok {
			result[id] = append([]message.Message{}, msgs...)
		}
	}
	return result, nil
}

func (w *cmdDrivingWorkspace) ListUserMessages(_ context.Context, _ string) ([]message.Message, error) {
	return nil, nil
}

func (w *cmdDrivingWorkspace) ListAllUserMessages(_ context.Context) ([]message.Message, error) {
	return nil, nil
}

func (w *cmdDrivingWorkspace) DeleteSession(_ context.Context, _ string) error {
	return nil
}

func (w *cmdDrivingWorkspace) SetCurrentSession(_ context.Context, sessionID string) error {
	return w.SetCurrentSessionGeneration(context.Background(), sessionID, 0)
}

func (w *cmdDrivingWorkspace) SetCurrentSessionGeneration(_ context.Context, sessionID string, _ uint64) error {
	w.setCurrentSessionCalls++
	w.currentSessionIDs = append(w.currentSessionIDs, sessionID)
	return nil
}

func (w *cmdDrivingWorkspace) SessionDescendantCost(context.Context, string) (float64, error) {
	return 0, nil
}

func (w *cmdDrivingWorkspace) SupportsThreads() bool { return w.supportsThreads }
func (w *cmdDrivingWorkspace) ListThreads(_ context.Context) ([]proto.Thread, error) {
	w.listThreadsCalls++
	return append([]proto.Thread(nil), w.threads...), nil
}
func (w *cmdDrivingWorkspace) SupportsTasks() bool { return false }
func (w *cmdDrivingWorkspace) ListTasks(context.Context) ([]proto.Thread, error) {
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

func (w *cmdDrivingWorkspace) LSPStart(ctx context.Context, path string) { w.lspStartCalls++ }
func (w *cmdDrivingWorkspace) LSPStopAll(ctx context.Context)            {}
func (w *cmdDrivingWorkspace) LSPGetStates() map[string]workspace.LSPClientInfo {
	return nil
}

func (w *cmdDrivingWorkspace) LSPGetDiagnosticCounts(name string) proto.LSPDiagnosticCounts {
	return proto.LSPDiagnosticCounts{}
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
func (w *cmdDrivingWorkspace) MCPGetStates() map[string]workspace.MCPClientInfo {
	return nil
}

func (w *cmdDrivingWorkspace) Stats(context.Context, stats.Request) (stats.Snapshot, error) {
	return stats.Snapshot{}, nil
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

func (w *cmdDrivingWorkspace) ListMCPPrompts(ctx context.Context) ([]workspace.MCPPrompt, error) {
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

func (w *cmdDrivingWorkspace) MCPPendingAuth() []workspace.MCPPendingAuthServer {
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
	w.listSessionHistoryCalls++
	return w.historyBySessionID[sessionID], nil
}

func (w *cmdDrivingWorkspace) PrepareSessionChanges(ctx context.Context, sessionID string) ([]workspace.SessionFile, error) {
	files, err := w.ListSessionHistory(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return workspace.AggregateSessionFiles(files), nil
}

func (w *cmdDrivingWorkspace) QuestionAnswer(batchID string, responses []question.Answer) bool {
	w.questionAnswerCalls++
	w.questionAnswerBatchID = batchID
	w.questionAnswerResponse = responses
	return false
}

func (w *cmdDrivingWorkspace) QuestionCancel() bool {
	w.questionCancelCalls++
	return false
}
func (w *cmdDrivingWorkspace) Subscribe(func(any)) {}
func (w *cmdDrivingWorkspace) Shutdown()           {}

func (w *cmdDrivingWorkspace) ListAccounts(string) ([]accounts.Account, error) {
	w.listAccountsCalls++
	return w.accs, w.listAccountsErr
}

func (w *cmdDrivingWorkspace) UpdateAccount(_ string, account accounts.Account) error {
	w.updateAccountCalls++
	w.lastUpdatedAccount = account
	return w.updateAccountErr
}

func (w *cmdDrivingWorkspace) SetProviderProxy(_, proxy string) error {
	w.setProviderProxyCalls++
	w.lastSetProviderProxy = proxy
	return w.setProviderProxyErr
}

func (w *cmdDrivingWorkspace) RemoveAccount(_ config.Scope, _, accountID string) error {
	w.removeAccountCalls++
	w.lastRemovedID = accountID
	return w.removeAccountErr
}

// ---------------------------------------------------------------------------
// cmdDrivenUI builds a UI over cmdDrivingWorkspace with all caches warm.
// ---------------------------------------------------------------------------

func newCmdDrivenUI(ws *cmdDrivingWorkspace) *UI {
	com := common.DefaultCommon(context.Background(), ws)
	return &UI{
		com: com,
		widgets: widgets{
			status: NewStatus(com, nil),
			chat:   chatlist.NewChat(com, config.ScrollbarDefault),
			dialog: dialog.NewOverlay(),
		},
		editor: editorState{
			textarea:    textarea.New(),
			attachments: attachments.New(nil, attachments.Keymap{}),
		},
		state: uiChat,
		focus: uiFocusEditor,
		lay: layoutState{
			width:  140,
			height: 45,
		},
		sess:   sessionState{current: &session.Session{ID: "s1"}},
		keyMap: DefaultKeyMap(),
	}
}

// warmCmdDrivenCaches marks all memoized workspace state fresh so only
// explicit state transitions (not startup staleness) trigger refresh dispatches.
func warmCmdDrivenCaches(m *UI) {
	m.wsCache.agentBusyCache.Set(false)
	m.wsCache.yoloCache.Set(false)
	m.wsCache.agentCache.Set(agentReadyModel{ready: true})
	m.promptQueue.cache.Set(m.promptQueue.cache.Value)
	m.lsp.checkedAt = time.Now()
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

// unwrapOwnedOne unwraps a single ownedMsg envelope (see root.go) to the
// message it carries, or returns msg unchanged if it isn't one. Test
// helpers that drive a bare *UI need this because ownCmd-wrapped results
// (cross-package types like chatlist.DelayedClickMsg that cannot embed
// uiOwned themselves) are only unwrapped by Root, which these helpers
// deliberately bypass — without it, *UI.Update receives an envelope type
// its own switch has no case for, and the real message never arrives.
func unwrapOwnedOne(msg tea.Msg) tea.Msg {
	if env, ok := msg.(ownedMsg); ok {
		return env.inner
	}
	return msg
}

// driveCmdStep executes one leaf-producing command and immediately routes its
// message through Update. It deliberately does not execute Update's returned
// command, allowing tests to inspect state at asynchronous boundaries.
func driveCmdStep(m *UI, cmd tea.Cmd) (tea.Msg, tea.Cmd) {
	msg := unwrapOwnedOne(cmd())
	_, next := m.Update(msg)
	return msg, next
}

// selfPerpetuatingTick reports whether msg is an animation tick, whose
// handler answers with the command that schedules the next one. Draining a
// command tree means following every returned command to its end, and an
// animation loop by design has no end: enqueueing the next tick makes these
// helpers spin (sleeping out each tick's delay) until the test times out.
// They run one such tick through Update — the state change is what tests
// care about — and stop there.
func selfPerpetuatingTick(msg tea.Msg) bool {
	switch msg.(type) {
	case spinner.TickMsg, spin.StepMsg:
		return true
	default:
		return false
	}
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
		msg := unwrapOwnedOne(cmd())
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
		if next != nil && !selfPerpetuatingTick(msg) {
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
	m.wsCache.agentBusyCache.Set(true)
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
	// The owner tag rides along on every dispatched probe (see uiOwnedMsg),
	// so the expected value carries it too.
	require.Contains(t, messages, busyStateMsg{
		uiOwned:   uiOwned{owner: m},
		gen:       m.wsCache.busyFetchGen,
		ready:     true,
		agentBusy: true,
	}, "the replacement command must execute and deliver its result")
}

// ---------------------------------------------------------------------------
// Test: Repeated Enter — message send via UI.Update → busy state optimistic
// update → re-fetch.
// ---------------------------------------------------------------------------

func TestCmdDriving_RepeatedEnter_SendAndSubmit(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{
		agentReady:          true,
		agentErr:            nil,
		sessionsBySessionID: map[string]session.Session{"new-sess": {ID: "new-sess"}},
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
	m.permissionResponse.open(perm.ID, false)
	m.dialog.OpenDialog(permsDialog)

	require.True(t, m.dialog.ContainsDialog(dialog.PermissionsID),
		"permissions dialog must be open")

	// Simulate pressing 'a' (Allow) through the real Update path:
	// UI.Update → tea.KeyPressMsg → handleKeyPressMsg →
	// handleDialogMsg → permsDialog.HandleMsg → ActionPermissionResponse →
	// applyDialogAction (side effects: PermissionGrant + CloseDialog).
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	// A second answer before the first command returns is rejected. It must not
	// make another workspace call or advance the response lifecycle.
	_, duplicateCmd := m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	duplicateMessages := runCmdTree(m, duplicateCmd, nil)
	var warning util.InfoMsg
	for _, msg := range duplicateMessages {
		if info, ok := msg.(util.InfoMsg); ok && info.Type == util.InfoTypeWarn {
			warning = info
			break
		}
	}
	require.Equal(t, util.InfoTypeWarn, warning.Type)
	require.Equal(t, "Permission response is already being submitted", warning.Msg)
	require.Zero(t, ws.permGrantCalls)

	// The permission action dispatches a tea.Cmd (PermissionGrant) so the
	// I/O is not done on the Update goroutine. Drive the original command to
	// verify the one workspace call it is allowed to make.
	require.NotNil(t, cmd, "allow action dispatches a cmd for the I/O")
	messages := runCmdTree(m, cmd, nil)
	// Verify the cmd produced no error.
	for _, msg := range messages {
		if info, ok := msg.(util.InfoMsg); ok {
			if info.Type == util.InfoTypeError {
				t.Fatalf("permission allow returned error: %s", info.Msg)
			}
		}
	}

	// Verify workspace call fired.
	require.GreaterOrEqual(t, ws.permGrantCalls, 1,
		"workspace.PermissionGrant must be called after Allow action")

	// Dialog must close.
	require.False(t, m.dialog.ContainsDialog(dialog.PermissionsID),
		"permissions dialog must close after action")
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
	m.wsCache.agentCache.Value.ready = false
	_, cmd := m.Update(busyStateMsg{gen: m.wsCache.busyFetchGen, ready: true, agentBusy: false})
	_ = runCmdTree(m, cmd, nil)
	require.True(t, m.wsCache.agentCache.Value.ready, "busyStateMsg must route to applyBusyState")

	// 2. promptQueueMsg → applyPromptQueue path.
	_, cmd = m.Update(promptQueueMsg{forSession: "s1", gen: m.promptQueue.cache.Generation, prompts: []string{"p1"}})
	_ = runCmdTree(m, cmd, nil)
	require.Equal(t, 1, len(m.promptQueue.cache.Value), "promptQueueMsg must update queue")

	// 3. agentRunSubmittedMsg → invalidation + re-dispatch path.
	m.wsCache.agentBusyCache.Set(true)
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
	staleGen := m.promptQueue.cache.Generation
	staleCmd := func() tea.Msg {
		return promptQueueMsg{
			forSession: "s2",
			gen:        staleGen,
			prompts:    []string{"stale"},
		}
	}

	_, freshCmd := driveCmdStep(m, staleCmd)
	require.Zero(t, len(m.promptQueue.cache.Value),
		"result from different session must not populate queue before refresh runs")
	require.NotNil(t, freshCmd, "session-mismatched result must schedule a refresh")
	require.True(t, m.promptQueue.cache.InFlight,
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
		agentReady: true,
		agentErr:   nil,
		sessionsBySessionID: map[string]session.Session{
			"sess-New Session": {ID: "sess-New Session", Title: "New Session"},
		},
	}
	// No session set — m.sess.current is nil.
	m := newCmdDrivenUI(ws)
	m.sess.current = nil
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

	// Session creation now produces createSessionMsg which Update handles
	// by setting the session and dispatching requestSessionLoad + sendMessage.
	// The cmd tree follows both, so we should see loadSessionMsg and
	// agentRunSubmittedMsg in the message stream.
	var gotLoadSession, gotRunSubmitted bool
	for _, msg := range messages {
		if _, ok := msg.(loadSessionMsg); ok {
			gotLoadSession = true
		}
		if _, ok := msg.(agentRunSubmittedMsg); ok {
			gotRunSubmitted = true
		}
	}
	require.True(t, gotLoadSession, "session creation must trigger loadSessionMsg")
	require.True(t, gotRunSubmitted, "must produce agentRunSubmittedMsg")
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
	m.permissionResponse.open(perm.ID, false)
	m.dialog.OpenDialog(permsDialog)

	// Simulate pressing 'd' (Deny) through the real Update path:
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	// The permission action dispatches a tea.Cmd (PermissionDeny).
	require.NotNil(t, cmd, "deny action dispatches a cmd for the I/O")
	_ = runCmdTree(m, cmd, nil)

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
	m.permissionResponse.open(perm.ID, false)
	// Open the permissions dialog (use OpenDialog to skip the grace period).
	m.dialog.OpenDialog(permsDialog)

	// Press 's' (Allow for session) through the real Update path:
	_, cmd := m.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	// The permission action dispatches a tea.Cmd (PermissionGrantPersistent).
	require.NotNil(t, cmd, "allow-for-session action dispatches a cmd for the I/O")
	_ = runCmdTree(m, cmd, nil)

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

func TestCmdDriving_PreviewResultRoutedToCoveredFilePicker(t *testing.T) {
	ws := &cmdDrivingWorkspace{agentReady: true}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)
	m.dialog.OpenDialog(&stubActionDialog{
		id:     dialog.FilePickerID,
		action: dialog.ActionCmd{Cmd: tea.Raw("terminal-output")},
	})
	m.dialog.OpenDialog(&stubActionDialog{id: "top"})

	messages := runCmdTree(m, func() tea.Msg { return fimage.PreviewPreparedMsg{} }, nil)

	require.Contains(t, messages, tea.RawMsg{Msg: "terminal-output"})
}

func TestCmdDriving_LoadSession_FreshResultApplied(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{
		agentReady: true,
		sessionsBySessionID: map[string]session.Session{
			"s-new": {ID: "s-new", Title: "New Session"},
		},
		messagesBySessionID: map[string][]message.Message{
			"s-new": {
				{ID: "m1", Role: message.User, SessionID: "s-new", Parts: []message.ContentPart{
					message.TextContent{Text: "hello"},
				}},
			},
		},
	}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	_, cmd := m.Update(requestSessionLoad{sessionID: "s-new"})
	require.NotNil(t, cmd, "requestSessionLoad must produce a cmd")

	messages := runCmdTree(m, cmd, nil)

	var got loadSessionMsg
	for _, msg := range messages {
		if lsm, ok := msg.(loadSessionMsg); ok {
			got = lsm
		}
	}
	require.True(t, got.gen > 0, "must receive loadSessionMsg with generation")
	require.Equal(t, "s-new", got.sessionID)
	require.Equal(t, "New Session", got.session.Title)
	require.Len(t, got.items, 1)
	require.Equal(t, "s-new", m.sess.current.ID, "session must be set")
	require.GreaterOrEqual(t, m.chat.Len(), 1, "chat must contain the loaded message")
	require.GreaterOrEqual(t, ws.agentReadyCalls, 1, "must probe AgentIsReady")
	require.True(t, m.wsCache.agentCache.Value.ready, "ready state must be cached")
}

func TestCmdDriving_LoadSession_NestedToolsApplied(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{
		agentReady: true,
		sessionsBySessionID: map[string]session.Session{
			"parent": {ID: "parent"},
		},
		messagesBySessionID: map[string][]message.Message{
			"parent": {{
				ID: "parent-message", Role: message.Assistant, SessionID: "parent",
				Parts: []message.ContentPart{message.ToolCall{ID: "agent-call", Name: "agent", Input: `{}`}},
			}},
			"parent-message$$agent-call": {{
				ID: "child-message", Role: message.Assistant, SessionID: "parent-message$$agent-call",
				Parts: []message.ContentPart{message.ToolCall{ID: "child-call", Name: "bash", Input: `{}`}},
			}},
		},
	}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	_, cmd := m.Update(requestSessionLoad{sessionID: "parent"})
	result := cmd().(loadSessionMsg)
	require.NoError(t, result.err)
	require.Len(t, result.items, 1)
	container, ok := result.items[0].(chat.NestedToolContainer)
	require.True(t, ok)
	require.Len(t, container.NestedTools(), 1)
	require.Equal(t, "child-call", container.NestedTools()[0].ToolCall().ID)

	m.Update(result)
	item := m.chat.MessageItem("agent-call")
	require.NotNil(t, item, "the delegation container must be in the chat")
	applied, ok := item.(chat.NestedToolContainer)
	require.True(t, ok)
	require.Len(t, applied.NestedTools(), 1)
	require.Equal(t, "child-call", applied.NestedTools()[0].ToolCall().ID)
}

func TestCmdDriving_LoadSession_MultipleChildrenBatched(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{
		agentReady: true,
		sessionsBySessionID: map[string]session.Session{
			"parent": {ID: "parent"},
		},
		messagesBySessionID: map[string][]message.Message{
			"parent": {
				{
					ID: "parent-message", Role: message.Assistant, SessionID: "parent",
					Parts: []message.ContentPart{
						message.ToolCall{ID: "agent-1", Name: "agent", Input: `{}`},
						message.ToolCall{ID: "agent-2", Name: "agent", Input: `{}`},
						message.ToolCall{ID: "bash-1", Name: "bash", Input: `{}`},
					},
				},
			},
			"parent-message$$agent-1": {
				{
					ID: "child1-msg", Role: message.Assistant, SessionID: "parent-message$$agent-1",
					Parts: []message.ContentPart{message.ToolCall{ID: "child1-call", Name: "bash", Input: `{}`}},
				},
			},
			"parent-message$$agent-2": {
				{
					ID: "child2-msg", Role: message.Assistant, SessionID: "parent-message$$agent-2",
					Parts: []message.ContentPart{message.ToolCall{ID: "child2-call", Name: "bash", Input: `{}`}},
				},
			},
		},
	}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	_, cmd := m.Update(requestSessionLoad{sessionID: "parent"})
	result := cmd().(loadSessionMsg)
	require.NoError(t, result.err)

	require.Len(t, result.items, 3)

	var agentContainers []chat.NestedToolContainer
	for _, item := range result.items {
		if container, ok := item.(chat.NestedToolContainer); ok {
			agentContainers = append(agentContainers, container)
		}
	}
	require.Len(t, agentContainers, 2, "should have 2 agent containers")

	for _, container := range agentContainers {
		nested := container.NestedTools()
		require.Len(t, nested, 1)
	}

	require.Equal(t, 1, ws.listMessagesCalls)
	require.Equal(t, 1, ws.listMessagesByIDsCalls)
	require.ElementsMatch(t, []string{"parent-message$$agent-1", "parent-message$$agent-2"}, ws.listMessagesByIDs[0])
}

func TestCmdDriving_LoadSession_DeepNestingRecursiveBatch(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{
		agentReady: true,
		sessionsBySessionID: map[string]session.Session{
			"parent": {ID: "parent"},
		},
		messagesBySessionID: map[string][]message.Message{
			"parent": {
				{
					ID: "msg-level1", Role: message.Assistant, SessionID: "parent",
					Parts: []message.ContentPart{
						message.ToolCall{ID: "agent-level1", Name: "agent", Input: `{}`},
					},
				},
			},
			"msg-level1$$agent-level1": {
				{
					ID: "msg-level2", Role: message.Assistant, SessionID: "msg-level1$$agent-level1",
					Parts: []message.ContentPart{
						message.ToolCall{ID: "agent-level2", Name: "agent", Input: `{}`},
					},
				},
			},
			"msg-level2$$agent-level2": {
				{
					ID: "msg-level3", Role: message.Assistant, SessionID: "msg-level2$$agent-level2",
					Parts: []message.ContentPart{message.ToolCall{ID: "deep-call", Name: "bash", Input: `{}`}},
				},
			},
		},
	}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	_, cmd := m.Update(requestSessionLoad{sessionID: "parent"})
	result := cmd().(loadSessionMsg)
	require.NoError(t, result.err)

	require.Len(t, result.items, 1)
	level1Container := result.items[0].(chat.NestedToolContainer)
	level1Nested := level1Container.NestedTools()
	require.Len(t, level1Nested, 1)

	level2Container := level1Nested[0].(chat.NestedToolContainer)
	level2Nested := level2Container.NestedTools()
	require.Len(t, level2Nested, 1)

	require.Equal(t, "deep-call", level2Nested[0].ToolCall().ID)
	require.Equal(t, 1, ws.listMessagesCalls)
	require.Equal(t, 2, ws.listMessagesByIDsCalls)
	require.Equal(t, []string{"msg-level1$$agent-level1"}, ws.listMessagesByIDs[0])
	require.Equal(t, []string{"msg-level2$$agent-level2"}, ws.listMessagesByIDs[1])
}

func TestCmdDriving_LoadSession_CapturesWorkspace(t *testing.T) {
	t.Parallel()

	original := &cmdDrivingWorkspace{
		sessionsBySessionID: map[string]session.Session{
			"captured": {ID: "captured", Title: "Captured"},
		},
	}
	replacement := &cmdDrivingWorkspace{
		sessionsBySessionID: map[string]session.Session{
			"captured": {ID: "captured", Title: "Replacement"},
		},
	}
	m := newCmdDrivenUI(original)
	warmCmdDrivenCaches(m)

	_, cmd := m.Update(requestSessionLoad{sessionID: "captured"})
	m.com.Workspace = replacement

	msg := cmd().(loadSessionMsg)
	require.NoError(t, msg.err)
	require.Equal(t, "Captured", msg.session.Title)
	require.Equal(t, 1, original.getSessionCalls)
	require.Zero(t, replacement.getSessionCalls)
}

func TestCmdDriving_LoadSession_StaleResultDiscarded(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{
		agentReady: true,
		sessionsBySessionID: map[string]session.Session{
			"s-old":   {ID: "s-old", Title: "Old Session"},
			"s-fresh": {ID: "s-fresh", Title: "Fresh Session"},
		},
		messagesBySessionID: map[string][]message.Message{
			"s-old":   {{ID: "old", Role: message.User, SessionID: "s-old", Parts: []message.ContentPart{message.TextContent{Text: "old"}}}},
			"s-fresh": {{ID: "fresh", Role: message.User, SessionID: "s-fresh", Parts: []message.ContentPart{message.TextContent{Text: "fresh"}}}},
		},
		historyBySessionID: map[string][]history.File{
			"s-fresh": {{SessionID: "s-fresh", Path: "fresh.go", Content: "fresh", Version: 1}},
		},
	}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	_, oldCmd := m.Update(requestSessionLoad{sessionID: "s-old"})
	_, freshCmd := m.Update(requestSessionLoad{sessionID: "s-fresh"})
	oldResult := oldCmd().(loadSessionMsg)
	runCmdTree(m, freshCmd, nil)

	sessionID, sessionTitle := m.sess.current.ID, m.sess.current.Title
	chatLen, filesLen := m.chat.Len(), len(m.sess.files)
	lspCalls, historyCalls := ws.lspStartCalls, ws.listSessionHistoryCalls
	presenceCalls, errorInfo := ws.setCurrentSessionCalls, m.status.InfoMsg()

	_, staleCmd := m.Update(oldResult)
	require.Nil(t, staleCmd)
	require.Equal(t, sessionID, m.sess.current.ID)
	require.Equal(t, sessionTitle, m.sess.current.Title)
	require.Equal(t, chatLen, m.chat.Len())
	require.Equal(t, filesLen, len(m.sess.files))
	require.Equal(t, lspCalls, ws.lspStartCalls)
	require.Equal(t, historyCalls, ws.listSessionHistoryCalls)
	require.Equal(t, presenceCalls, ws.setCurrentSessionCalls)
	require.Equal(t, errorInfo, m.status.InfoMsg())
}

func TestCmdDriving_LoadSession_ReportUsesAcceptedSessionAfterSupersedingRequest(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{
		agentReady: true,
		sessionsBySessionID: map[string]session.Session{
			"s-old":   {ID: "s-old"},
			"s-fresh": {ID: "s-fresh"},
		},
	}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	_, oldLoadCmd := m.Update(requestSessionLoad{sessionID: "s-old"})
	oldResult := oldLoadCmd().(loadSessionMsg)
	_, freshLoadCmd := m.Update(requestSessionLoad{sessionID: "s-fresh"})
	freshResult := freshLoadCmd().(loadSessionMsg)
	_, reportCmd := m.Update(freshResult)
	require.NotNil(t, reportCmd)

	m.Update(requestSessionLoad{sessionID: "s-next"})
	runCmdTree(m, reportCmd, nil)
	require.Equal(t, []string{"s-fresh"}, ws.currentSessionIDs)

	_, staleCmd := m.Update(oldResult)
	require.Nil(t, staleCmd)
	require.Equal(t, []string{"s-fresh"}, ws.currentSessionIDs)
}

func TestCmdDriving_LoadSession_StaleErrorDiscarded(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{
		agentReady: true,
		sessionsBySessionID: map[string]session.Session{
			"s-old":   {ID: "s-old"},
			"s-fresh": {ID: "s-fresh"},
		},
		messagesBySessionID: map[string][]message.Message{
			"s-fresh": {{ID: "fresh", Role: message.User, SessionID: "s-fresh", Parts: []message.ContentPart{message.TextContent{Text: "fresh"}}}},
		},
		listMessagesErrByID: map[string]error{"s-old": fmt.Errorf("old load failed")},
	}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	_, oldCmd := m.Update(requestSessionLoad{sessionID: "s-old"})
	_, freshCmd := m.Update(requestSessionLoad{sessionID: "s-fresh"})
	oldResult := oldCmd().(loadSessionMsg)
	require.Error(t, oldResult.err)
	runCmdTree(m, freshCmd, nil)

	sessionID, chatLen, errorInfo := m.sess.current.ID, m.chat.Len(), m.status.InfoMsg()
	presenceCalls, lspCalls, historyCalls := ws.setCurrentSessionCalls, ws.lspStartCalls, ws.listSessionHistoryCalls
	_, staleCmd := m.Update(oldResult)
	require.Nil(t, staleCmd)
	require.Equal(t, sessionID, m.sess.current.ID)
	require.Equal(t, chatLen, m.chat.Len())
	require.Equal(t, errorInfo, m.status.InfoMsg())
	require.Equal(t, presenceCalls, ws.setCurrentSessionCalls)
	require.Equal(t, lspCalls, ws.lspStartCalls)
	require.Equal(t, historyCalls, ws.listSessionHistoryCalls)
}

// ---------------------------------------------------------------------------
// Test: sendMessage when agent not ready — AgentReadyErr blocks the send.
// ---------------------------------------------------------------------------

func TestCmdDriving_SendMessage_AgentNotReady(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{
		agentReady:          false,
		agentErr:            fmt.Errorf("agent not initialized"),
		sessionsBySessionID: map[string]session.Session{"s1": {ID: "s1"}},
	}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	_, cmd := m.Update(sendMessageMsg{Content: "test"})
	require.NotNil(t, cmd)
	messages := runCmdTree(m, cmd, nil)

	// Must have gotten an error info message, not agentRunSubmittedMsg.
	var gotError bool
	for _, msg := range messages {
		if info, ok := msg.(util.InfoMsg); ok && info.Type == util.InfoTypeError {
			gotError = true
		}
	}
	require.True(t, gotError, "must surface AgentReadyErr as InfoMsg")

	// AgentRun must NOT have been called.
	require.Zero(t, ws.agentRunCalls)
	// CreateSession must NOT have been called.
	require.Zero(t, ws.createSessionCalls)
}

// ---------------------------------------------------------------------------
// Test: sendMessage CreateSession failure — error surfaces, no AgentRun.
// ---------------------------------------------------------------------------

func TestCmdDriving_SendMessage_CreateSessionError(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{
		agentReady: true,
		agentErr:   nil,
		// No sessions registered and CreateSession returns error.
	}
	ws.createSessionErr = fmt.Errorf("DB full")
	m := newCmdDrivenUI(ws)
	m.sess.current = nil // No session → triggers CreateSession
	warmCmdDrivenCaches(m)

	_, cmd := m.Update(sendMessageMsg{Content: "test"})
	require.NotNil(t, cmd)
	messages := runCmdTree(m, cmd, nil)

	var gotError bool
	for _, msg := range messages {
		if info, ok := msg.(util.InfoMsg); ok && info.Type == util.InfoTypeError {
			gotError = true
		}
	}
	require.True(t, gotError, "must surface CreateSession error")
	require.Zero(t, ws.agentRunCalls)
}

// ---------------------------------------------------------------------------
// Test: sendMessage with existing session — no session creation, direct AgentRun.
// ---------------------------------------------------------------------------

func TestCmdDriving_SendMessage_SessionExists(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{
		agentReady:          true,
		agentErr:            nil,
		sessionsBySessionID: map[string]session.Session{"s1": {ID: "s1"}},
	}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	_, cmd := m.Update(sendMessageMsg{Content: "hello"})
	require.NotNil(t, cmd)
	messages := runCmdTree(m, cmd, nil)

	// No session creation should happen.
	require.Zero(t, ws.createSessionCalls)

	// AgentRun should be called.
	require.Equal(t, 1, ws.agentRunCalls)

	// Must produce agentRunSubmittedMsg.
	var gotRunSubmitted bool
	for _, msg := range messages {
		if _, ok := msg.(agentRunSubmittedMsg); ok {
			gotRunSubmitted = true
		}
	}
	require.True(t, gotRunSubmitted)
}

// ---------------------------------------------------------------------------
// Test: SetCompactMode runs in a cmd — toggleCompactMode returns a cmd.
// ---------------------------------------------------------------------------

func TestCmdDriving_ToggleCompactMode_InCmd(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	cmd := m.toggleCompactMode()
	require.NotNil(t, cmd, "toggleCompactMode must return a cmd for SetCompactMode I/O")

	messages := runCmdTree(m, cmd, nil)
	// The cmd returns nil (success), so no messages should be errors.
	for _, msg := range messages {
		if info, ok := msg.(util.InfoMsg); ok && info.Type == util.InfoTypeError {
			t.Fatalf("unexpected error: %s", info.Msg)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: PermissionGrant runs in a cmd — workspace call is off the Update goroutine.
// ---------------------------------------------------------------------------

func TestCmdDriving_PermissionGrant_InCmd(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	m := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(m)

	// Directly dispatch the permission cmd.
	perm := permission.PermissionRequest{ID: "p1", ToolName: "bash"}
	cmd := func() tea.Msg {
		m.com.Workspace.PermissionGrant(perm)
		return nil
	}

	messages := runCmdTree(m, cmd, nil)
	require.Equal(t, 1, ws.permGrantCalls, "PermissionGrant must fire in the cmd")
	for _, msg := range messages {
		if info, ok := msg.(util.InfoMsg); ok && info.Type == util.InfoTypeError {
			t.Fatalf("unexpected error: %s", info.Msg)
		}
	}
}
