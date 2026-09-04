package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/ui/chatlist"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/threads"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// rootTestWorkspace is a minimal workspace.Workspace stub sized for driving
// Root/New end to end: unlike threadsTestWorkspace (which only exercises
// the cache's ListThreads round trip), New() also touches
// PermissionSkipRequests/AgentIsReady/Config on construction, so those need
// stubbing here too.
type rootTestWorkspace struct {
	workspace.Workspace
	supportsThreads bool
	// listThreadsCalls counts ListThreads round trips, for
	// TestThreadEventDispatchesOneListThreadsCall (threads_rpc_collapse_test.go).
	listThreadsCalls int
}

// KnownProviders: no test here renders a provider list.
func (w rootTestWorkspace) KnownProviders() []catwalk.Provider { return nil }

// SkillStates, BuiltinSkills: the skills panel reads these; no test
// here has a catalog beyond what the binary ships.
func (w rootTestWorkspace) SkillStates() []*skills.SkillState { return nil }
func (w rootTestWorkspace) ConfigProblems() []config.Problem  { return nil }
func (w rootTestWorkspace) BuiltinSkills() []*skills.Skill    { return skills.DiscoverBuiltin() }

func (w *rootTestWorkspace) Config() *config.Config {
	providers := csync.NewMap[string, config.ProviderConfig]()
	// New() must land in uiLanding/uiFocusEditor (not uiOnboarding) so
	// handleGlobalKeys — and therefore the threads key — is reachable; that
	// needs at least one enabled provider (see Config.IsConfigured).
	providers.Set("test-provider", config.ProviderConfig{ID: "test-provider"})
	return &config.Config{
		Providers: providers,
		Options:   &config.Options{TUI: &config.TUIOptions{}},
	}
}

func (w *rootTestWorkspace) PermissionSkipRequests() bool { return false }
func (w *rootTestWorkspace) AgentIsReady() bool           { return false }
func (w *rootTestWorkspace) SupportsThreads() bool        { return w.supportsThreads }

// QuestionCancel overrides the embedded nil Workspace: detaching an
// attached thread now cancels any question still pending on it (see
// cancelThreadQuestion), so any test that builds an embedded thread UI
// around this stub calls this on teardown.
func (w *rootTestWorkspace) QuestionCancel() bool { return false }

// SupportsTasks answers for the delegation list behind the panel's
// agents section; no test here drives one.
func (w *rootTestWorkspace) SupportsTasks() bool                       { return false }
func (w *rootTestWorkspace) WorkingDir() string                        { return "/tmp" }
func (w *rootTestWorkspace) ProjectNeedsInitialization() (bool, error) { return false, nil }

func (w *rootTestWorkspace) ListThreads(context.Context) ([]proto.Thread, error) {
	w.listThreadsCalls++
	return nil, nil
}

type neutralSubscriberWorkspace struct {
	rootTestWorkspace
	send      func(any)
	stopped   bool
	stopCalls int
}

func (w *neutralSubscriberWorkspace) SubscribeWith(send func(any)) func() {
	w.send = send
	return func() {
		w.stopped = true
		w.stopCalls++
	}
}

func (w *neutralSubscriberWorkspace) emit(msg any) {
	if !w.stopped && w.send != nil {
		w.send(msg)
	}
}

// newTestRoot builds a Root over rootTestWorkspace, configured so New()
// lands in uiLanding/uiFocusEditor where the global key switch (and
// therefore the threads key) is reachable.
func newTestRoot(t *testing.T, supportsThreads bool) *Root {
	t.Helper()
	ws := &rootTestWorkspace{supportsThreads: supportsThreads}
	com := common.DefaultCommon(context.Background(), ws)
	// Pin the platform so the ctrl+ key this test drives matches what
	// configuredKeyMap actually binds, regardless of the host OS running
	// the suite (see keys.go's darwin ctrl+ -> super+ rewrite).
	return NewRoot(com, "", false, withGOOS("linux"))
}

func ctrlE() tea.KeyPressMsg {
	return tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'e', Text: ""}
}

func TestViewEnablesAllMouseMotionForHoverFeedback(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, false)
	require.Equal(t, tea.MouseModeAllMotion, r.main.View().MouseMode)
}

// drainShowDashboard runs cmd the way the Bubble Tea runtime would (unwrapping
// tea.BatchMsg) until it finds the showThreadsDashboardMsg that
// UI.handleGlobalKeys produces for the threads key, feeding it back into
// r.Update. Other leaf messages are executed for side effects and dropped,
// mirroring runCmds in session_busy_test.go.
func drainShowDashboard(t *testing.T, r *Root, cmd tea.Cmd) *Root {
	t.Helper()
	if cmd == nil {
		return r
	}
	// Alongside the threads key, handleGlobalKeys' surrounding Update pass
	// also kicks off unrelated background refreshes (LSP, busy state, ...)
	// that rootTestWorkspace's minimal stub doesn't implement. Those leaves
	// aren't what this test is after, so a panic from one is swallowed —
	// only the showThreadsDashboardMsg branch matters here.
	msg := safeRunCmd(cmd)
	switch msg := msg.(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			r = drainShowDashboard(t, r, c)
		}
		return r
	case showThreadsDashboardMsg:
		model, next := r.Update(msg)
		r = model.(*Root)
		return drainShowDashboard(t, r, next)
	default:
		return r
	}
}

// safeRunCmd runs cmd, recovering (and returning nil) if it panics on a
// workspace method the test stub doesn't implement.
func safeRunCmd(cmd tea.Cmd) (msg tea.Msg) {
	defer func() {
		if recover() != nil {
			msg = nil
		}
	}()
	return cmd()
}

// TestThreadsKeyIgnoredWhenUnsupported pins that pressing the threads
// toggle never switches screens when the workspace doesn't support
// threads: the check lives inside UI.handleGlobalKeys, reached because
// screenMain forwards keys straight to r.main.
func TestThreadsKeyIgnoredWhenUnsupported(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, false)
	_, cmd := r.Update(ctrlE())
	// drainShowDashboard only switches screens on showThreadsDashboardMsg;
	// with SupportsThreads() false, UI.handleGlobalKeys reports an info
	// message instead (see the ui.go Threads case), so this must be a
	// no-op regardless of whatever leaf messages the cmd tree contains.
	r = drainShowDashboard(t, r, cmd)
	require.Equal(t, screenMain, r.active)
}

// TestThreadsKeyTogglesDashboard drives ctrl+e main -> dashboard -> main.
func TestThreadsKeyTogglesDashboard(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)

	// main -> dashboard: ctrl+e is handled by UI.handleGlobalKeys, which
	// returns a cmd carrying showThreadsDashboardMsg; Root must apply it.
	_, cmd := r.Update(ctrlE())
	require.NotNil(t, cmd)
	r = drainShowDashboard(t, r, cmd)
	require.Equal(t, screenDashboard, r.active)
	require.NotNil(t, r.dashboard)

	// dashboard -> main: ctrl+e is intercepted directly by Root before
	// reaching the dashboard's own key handling.
	model, _ := r.Update(ctrlE())
	r = model.(*Root)
	require.Equal(t, screenMain, r.active)
}

// TestWindowSizeBroadcastsToDashboard confirms a WindowSizeMsg reaches both
// the main UI and an already-constructed dashboard.
func TestWindowSizeBroadcastsToDashboard(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	r.dashboard = threads.New(r.com, &r.main.threadList)

	model, _ := r.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	r = model.(*Root)

	require.Equal(t, 120, r.width)
	require.Equal(t, 40, r.height)
	require.Equal(t, 120, r.main.lay.width)
	width, height := r.dashboard.Size()
	require.Equal(t, 120, width)
	require.Equal(t, 40, height)
}

// TestDashboardCoalescedWheelScrolls is the regression test for the mouse
// wheel doing nothing over the threads dashboard: the input filter
// (cmd/root.go) rewrites every raw tea.MouseWheelMsg into
// common.CoalescedWheelMsg before Root ever sees it, but handleDashboardMsg
// used to switch only on the raw type, so threadsDashboard.HandleMouseWheel
// was dead code in production.
func TestDashboardCoalescedWheelScrolls(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	rows := make([]proto.Thread, 40)
	for i := range rows {
		rows[i] = proto.Thread{
			ID: fmt.Sprintf("t%d", i), Name: fmt.Sprintf("thread %d", i),
			Kind: "thread", Status: "running",
		}
	}
	r.main.threadList.Cache.Value = rows

	model, _ := r.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	r = model.(*Root)
	r.dashboard = threads.New(r.com, &r.main.threadList)
	r.dashboard.SetSize(120, 40)
	r.dashboard.RebuildItems()
	r.active = screenDashboard

	// The dashboard's scroll offset is its own package's business, so this
	// asserts on what the person actually sees: a list that scrolled shows
	// different rows than one that did not.
	before := r.View().Content

	model, _ = r.Update(common.CoalescedWheelMsg{
		Mouse:  tea.Mouse{X: 4, Y: 10},
		DeltaY: 3,
	})
	r = model.(*Root)

	require.NotEqual(t, before, r.View().Content,
		"the coalesced wheel event must reach the dashboard and scroll its list")
}

// TestDashboardDialogReceivesPaste is a regression test: handleDashboardMsg
// forwarded only mouse and wheel events to dashboardDialog and dropped
// everything else, so pasting a multi-line goal into the thread-create
// dialog's text inputs silently did nothing, while the main screen's
// dialogs kept receiving tea.PasteMsg normally.
func TestDashboardDialogReceivesPaste(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	model, _ := r.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	r = model.(*Root)
	_, cmd := r.Update(ctrlE())
	r = drainShowDashboard(t, r, cmd)
	require.Equal(t, screenDashboard, r.active)

	r.dashboardDialog.OpenDialog(dialog.NewThreadCreate(r.com))
	require.True(t, r.dashboardDialog.HasDialogs())

	before := r.View().Content
	require.NotContains(t, before, "pasted-goal-text")

	model, _ = r.Update(tea.PasteMsg{Content: "pasted-goal-text"})
	r = model.(*Root)

	require.Contains(t, r.View().Content, "pasted-goal-text",
		"a paste must reach the dashboard dialog's focused text input")
}

// TestThreadEventMsgDroppedWhenNotAttached exercises both "no thread
// attached" and "different thread attached" cases; neither should panic or
// misroute.
func TestThreadEventMsgDroppedWhenNotAttached(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)

	require.NotPanics(t, func() {
		_, cmd := r.Update(threadEventMsg{threadID: "s1", inner: tea.WindowSizeMsg{}})
		require.Nil(t, cmd)
	})

	threadUI := New(common.DefaultCommon(context.Background(), &rootTestWorkspace{}), "", false, WithEmbedded())
	r.attachment.thread = &threadAttachment{threadID: "s1", ui: threadUI}
	r.active = screenThread

	require.NotPanics(t, func() {
		_, cmd := r.Update(threadEventMsg{threadID: "s2", inner: tea.WindowSizeMsg{}})
		require.Nil(t, cmd)
	})
}

// TestThreadEventMsgReachesAttachedThread confirms a tagged event matching
// the currently attached thread is forwarded into that thread's own *UI.
func TestThreadEventMsgReachesAttachedThread(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	threadUI := New(common.DefaultCommon(context.Background(), &rootTestWorkspace{}), "", false, WithEmbedded())
	r.attachment.thread = &threadAttachment{threadID: "s1", ui: threadUI}
	r.active = screenThread

	_, cmd := r.Update(threadEventMsg{threadID: "s1", inner: tea.WindowSizeMsg{Width: 80, Height: 24}})
	if cmd != nil {
		cmd()
	}

	require.Equal(t, 80, threadUI.lay.width)
	require.Equal(t, 24, threadUI.lay.height)
}

func TestHandleThreadAttachedAdaptsNeutralSubscriber(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	ws := &neutralSubscriberWorkspace{}
	r.attachment.pendingID = "thread-1"
	detachCalls := 0
	delivered := make([]tea.Msg, 0, 1)
	r.SetSend(func(msg tea.Msg) {
		delivered = append(delivered, msg)
		model, _ := r.Update(msg)
		r = model.(*Root)
	})

	model, _ := r.handleThreadAttached(threadAttachedMsg{
		id:        "thread-1",
		sessionID: "session-1",
		name:      "thread",
		ws:        ws,
		detach:    func() { detachCalls++ },
	})
	r = model.(*Root)
	require.NotNil(t, ws.send)

	inner := tea.WindowSizeMsg{Width: 91, Height: 37}
	ws.emit(inner)
	require.Len(t, delivered, 1)
	event, ok := delivered[0].(threadEventMsg)
	require.True(t, ok)
	require.Equal(t, "thread-1", event.threadID)
	require.Equal(t, inner, event.inner)
	require.Equal(t, 91, r.attachment.thread.ui.lay.width)
	require.Equal(t, 37, r.attachment.thread.ui.lay.height)

	stop := r.detachThread()
	require.NotNil(t, stop)
	stop()
	require.Equal(t, 1, ws.stopCalls)
	require.Equal(t, 1, detachCalls)

	ws.emit(tea.WindowSizeMsg{Width: 12, Height: 8})
	require.Len(t, delivered, 1)
}

// TestAltUpAtThreadTopLevelReturnsToMain covers the new handleKeyPress
// branch: alt+up while attached to a thread, with nothing pushed onto that
// thread's own child-session nav stack, must return straight to
// screenMain (via leaveThreadToMain) instead of being forwarded into the
// thread's embedded UI, where it would otherwise be a no-op.
func TestAltUpAtThreadTopLevelReturnsToMain(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	threadUI := New(common.DefaultCommon(context.Background(), &rootTestWorkspace{}), "", false, WithEmbedded())
	r.attachment.thread = &threadAttachment{threadID: "s1", ui: threadUI}
	r.active = screenThread

	model, cmd := r.Update(tea.KeyPressMsg{Mod: tea.ModAlt, Code: tea.KeyUp})
	r = model.(*Root)
	if cmd != nil {
		cmd()
	}

	require.Equal(t, screenMain, r.active)
	require.Nil(t, r.attachment.thread, "leaveThreadToMain must tear down the attachment")
}

// TestLeaveThreadToMain covers the method directly: it tears down the
// attachment (stop/detach both run) and lands on screenMain, not
// screenDashboard the way leaveThread does.
func TestLeaveThreadToMain(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	threadUI := New(common.DefaultCommon(context.Background(), &rootTestWorkspace{}), "", false, WithEmbedded())

	stopped, detached := false, false
	r.attachment.thread = &threadAttachment{
		threadID: "s1",
		ui:       threadUI,
		stop:     func() { stopped = true },
		detach:   func() { detached = true },
	}
	r.active = screenThread

	cmd := r.leaveThreadToMain()
	require.Equal(t, screenMain, r.active)
	require.Nil(t, r.attachment.thread)
	require.False(t, stopped, "stop must run inside the returned cmd, not on the event loop")
	require.False(t, detached, "detach must run inside the returned cmd, not synchronously")

	require.NotNil(t, cmd)
	cmd()
	require.True(t, stopped)
	require.True(t, detached)
}

// TestDetachThreadDoesNotJoinTheEventPumpOnTheEventLoop is the regression
// test for a total TUI freeze: leaving a thread used to join its event pump
// synchronously, from inside Update. The pump delivers through
// tea.Program.Send, which parks until the event loop accepts the message,
// so a pump with a message in flight and an event loop waiting for that
// pump to exit is a cycle neither side can break — the whole UI stops
// responding while the threads behind it keep running.
//
// Here stop() stands in for that parked pump: it cannot return until the
// caller has moved on. Teardown must therefore not happen on the caller's
// goroutine.
func TestDetachThreadDoesNotJoinTheEventPumpOnTheEventLoop(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	threadUI := New(common.DefaultCommon(context.Background(), &rootTestWorkspace{}), "", false, WithEmbedded())

	release := make(chan struct{})
	stopReturned := make(chan struct{})
	r.attachment.thread = &threadAttachment{
		threadID: "s1",
		ui:       threadUI,
		stop: func() {
			<-release
			close(stopReturned)
		},
	}
	r.active = screenThread

	left := make(chan tea.Cmd, 1)
	go func() { left <- r.leaveThreadToMain() }()

	var cmd tea.Cmd
	select {
	case cmd = <-left:
	case <-time.After(5 * time.Second):
		t.Fatal("leaving a thread blocked the event loop on its own event pump")
	}
	require.Equal(t, screenMain, r.active)
	require.Nil(t, r.attachment.thread)

	// The teardown itself is allowed to block; it just may not do so where
	// the event loop can see it.
	require.NotNil(t, cmd)
	go cmd()
	close(release)
	select {
	case <-stopReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("the event pump was never stopped")
	}
}

// runBatchCmd executes cmd and, if it produced a tea.BatchMsg (as
// handleThreadAttached's tea.Batch of teardown/Init/resize cmds does), runs
// every non-nil cmd inside it too. Tests here only care that the batched
// teardown cmd ran, not about ordering; other leaves (e.g. childUI.Init's
// loadCustomCommands) touch workspace methods rootTestWorkspace doesn't
// implement, so those are run via safeRunCmd and any panic is swallowed,
// mirroring drainShowDashboard above.
func runBatchCmd(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msg := safeRunCmd(cmd)
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			runBatchCmd(t, c)
		}
	}
}

// TestHandleThreadAttachedTearsDownPreviousAttachment is the regression test
// for the leak in handleThreadAttached: a second threadAttachedMsg for a
// thread that's already attached (e.g. a double Enter on the same dashboard
// row) used to overwrite r.attachment.thread outright, leaking the first attachment's
// SubscribeWith pump goroutine and never releasing its workspace. Both
// teardown funcs on the first attachment must now run exactly once, and the
// second attachment must become the one installed.
func TestHandleThreadAttachedTearsDownPreviousAttachment(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	r.active = screenDashboard
	// The request itself (Enter on the dashboard row) is what marks the
	// answer as wanted; see Root.pendingAttach.
	model, _ := r.Update(threads.EnterMsg{ID: "s1", SessionID: "sess1", Name: "first"})
	r = model.(*Root)

	var firstStopCalls, firstDetachCalls, secondDetachCalls int
	firstWS := &rootTestWorkspace{}
	model, cmd := r.Update(threadAttachedMsg{
		id: "s1", sessionID: "sess1", name: "first", ws: firstWS,
		detach: func() { firstDetachCalls++ },
	})
	r = model.(*Root)
	// The fake workspace doesn't implement threadEventSubscriber, so stop()
	// stays the no-op default; wire our own counter onto the installed
	// attachment directly to observe it running through detachThread.
	r.attachment.thread.stop = func() { firstStopCalls++ }
	runBatchCmd(t, cmd)
	require.Equal(t, screenThread, r.active)
	require.NotNil(t, r.attachment.thread)
	first := r.attachment.thread

	secondWS := &rootTestWorkspace{}
	model, cmd = r.Update(threadAttachedMsg{
		id: "s1", sessionID: "sess1", name: "second", ws: secondWS,
		detach: func() { secondDetachCalls++ },
	})
	r = model.(*Root)
	require.NotSame(t, first, r.attachment.thread, "the second attachment must replace the first")
	require.Equal(t, screenThread, r.active)
	require.Equal(t, 0, firstStopCalls, "stop must run inside the returned cmd, not synchronously")
	require.Equal(t, 0, firstDetachCalls, "detach must run inside the returned cmd, not synchronously")

	require.NotNil(t, cmd)
	runBatchCmd(t, cmd)
	require.Equal(t, 1, firstStopCalls, "the first attachment's stop must run exactly once")
	require.Equal(t, 1, firstDetachCalls, "the first attachment's detach must run exactly once")
	require.Equal(t, 0, secondDetachCalls, "the second (still installed) attachment must not be detached")
}

// TestHandleThreadAttachedStaleAfterLeavingDashboard covers the second half
// of the leak fix: an attach response that lands after the user has already
// left the dashboard (esc back to screenMain) must not force the screen
// onto the thread — it releases the just-attached workspace instead.
func TestHandleThreadAttachedStaleAfterLeavingDashboard(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	r.active = screenMain

	detached := false
	model, cmd := r.Update(threadAttachedMsg{
		id: "s1", sessionID: "sess1", name: "late", ws: &rootTestWorkspace{},
		detach: func() { detached = true },
	})
	r = model.(*Root)

	require.Equal(t, screenMain, r.active, "a stale attach must not yank the screen")
	require.Nil(t, r.attachment.thread)
	require.False(t, detached, "detach must run inside the returned cmd, not synchronously")

	require.NotNil(t, cmd)
	runBatchCmd(t, cmd)
	require.True(t, detached, "the stale attachment's workspace must still be released")
}

// TestHandleThreadAttachedFromMainScreenPanel is the regression test for a
// click on a thread block in the main screen's session panel doing nothing:
// the attach result was judged wanted by "is the dashboard active?", so a
// request that started on screenMain (threads.EnterMsg from mouse.go) was
// treated as stale and its workspace silently released. The request the
// user is still waiting on (pendingAttach) must land whichever screen it
// started from.
func TestHandleThreadAttachedFromMainScreenPanel(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	r.active = screenMain

	model, cmd := r.Update(threads.EnterMsg{ID: "s1", SessionID: "sess1", Name: "panel"})
	r = model.(*Root)
	require.NotNil(t, cmd, "threads.EnterMsg must start an attach")
	require.Equal(t, "s1", r.attachment.pendingID)

	detached := false
	model, cmd = r.Update(threadAttachedMsg{
		id: "s1", sessionID: "sess1", name: "panel", ws: &rootTestWorkspace{},
		detach: func() { detached = true },
	})
	r = model.(*Root)
	runBatchCmd(t, cmd)

	require.Equal(t, screenThread, r.active, "the attach the user asked for must open the thread")
	require.NotNil(t, r.attachment.thread)
	require.Equal(t, "s1", r.attachment.thread.threadID)
	require.Empty(t, r.attachment.pendingID, "an answered request is no longer pending")
	require.False(t, detached, "the wanted attachment must stay installed, not be released")
}

// TestLeaveThreadFromMainScreenPanelBuildsDashboard is the regression test
// for a nil-pointer panic: a thread opened via enterThreadMsg (the main
// screen's session panel, not showThreadsDashboardMsg) never gets r.dashboard
// constructed, yet leaveThread (ctrl+e's screenThread case) unconditionally
// switched r.active to screenDashboard — View() then called
// r.dashboard.Draw() on nil. leaveThread must build the dashboard lazily,
// the same way showThreadsDashboardMsg does, before landing on it.
func TestLeaveThreadFromMainScreenPanelBuildsDashboard(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	r.active = screenMain

	model, _ := r.Update(threads.EnterMsg{ID: "s1", SessionID: "sess1", Name: "panel"})
	r = model.(*Root)
	model, cmd := r.Update(threadAttachedMsg{
		id: "s1", sessionID: "sess1", name: "panel", ws: &rootTestWorkspace{},
		detach: func() {},
	})
	r = model.(*Root)
	runBatchCmd(t, cmd)
	require.Equal(t, screenThread, r.active)
	require.Nil(t, r.dashboard, "no dashboard was ever opened for this thread")

	cmd = r.leaveThread()
	require.Equal(t, screenDashboard, r.active)
	require.NotNil(t, r.dashboard, "leaveThread must build one before landing on screenDashboard")
	if cmd != nil {
		runBatchCmd(t, cmd)
	}

	require.NotPanics(t, func() { r.View() })
}

// TestHandleThreadAttachedSupersededRequestIsStale: asking for a second
// thread before the first attach answers makes the first answer stale —
// only the latest request (pendingAttach) is wanted.
func TestHandleThreadAttachedSupersededRequestIsStale(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	r.active = screenDashboard

	model, _ := r.Update(threads.EnterMsg{ID: "s1", SessionID: "sess1", Name: "first"})
	r = model.(*Root)
	model, _ = r.Update(threads.EnterMsg{ID: "s2", SessionID: "sess2", Name: "second"})
	r = model.(*Root)

	detached := false
	model, cmd := r.Update(threadAttachedMsg{
		id: "s1", sessionID: "sess1", name: "first", ws: &rootTestWorkspace{},
		detach: func() { detached = true },
	})
	r = model.(*Root)
	require.Equal(t, screenDashboard, r.active, "a superseded attach must not take the screen")
	require.Nil(t, r.attachment.thread)
	runBatchCmd(t, cmd)
	require.True(t, detached, "the superseded attachment's workspace must be released")
	require.Equal(t, "s2", r.attachment.pendingID, "the latest request is still pending")
}

// TestChatWarmStepReachesMainScreenWhileThreadIsOpen is the regression
// test for the main screen losing its chat scrollbar for good: a terminal
// resize while a thread is drilled into still reaches the main UI
// (handleWindowSize broadcasts it), which puts its chat into the
// "resizing" state and arms a settle timer. That timer's chatlist.WarmMsg used
// to be routed by active screen — to the thread's UI — so the main chat
// never left "resizing", the state in which it skips the scrollbar. The
// warm step must come back to the UI that armed it, whichever screen is on
// top.
func TestChatWarmStepReachesMainScreenWhileThreadIsOpen(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	model, _ := r.Update(threads.EnterMsg{ID: "s1", SessionID: "sess1", Name: "x"})
	r = model.(*Root)
	model, cmd := r.Update(threadAttachedMsg{id: "s1", sessionID: "sess1", name: "x", ws: &rootTestWorkspace{}, detach: func() {}})
	r = model.(*Root)
	runBatchCmd(t, cmd)
	require.Equal(t, screenThread, r.active)

	r.main.state = uiChat
	model, cmd = r.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	r = model.(*Root)
	require.True(t, r.main.chat.Resizing(), "a resize must put the main chat into its resizing state")

	// Pull the main chat's settle timer out of the returned cmd tree and
	// feed its message back through Root, the way the runtime would.
	var warm *chatlist.WarmMsg
	var walk func(c tea.Cmd)
	walk = func(c tea.Cmd) {
		if c == nil {
			return
		}
		switch m := safeRunCmd(c).(type) {
		case tea.BatchMsg:
			for _, cc := range m {
				walk(cc)
			}
		case chatlist.WarmMsg:
			if m.Owner == r.main.chat {
				mm := m
				warm = &mm
			}
		}
	}
	walk(cmd)
	require.NotNil(t, warm, "the main chat must have armed a warm step")

	for i := 0; i < 8 && r.main.chat.Resizing(); i++ {
		model, cmd = r.Update(*warm)
		r = model.(*Root)
		walk(cmd)
	}
	require.False(t, r.main.chat.Resizing(), "the main chat must leave resizing even while a thread is on top")
	require.Equal(t, screenThread, r.active)
}

// activateThenAttachWorkspace records the order ActivateThread and
// AttachThread are called in, and lets a test control each call's error
// independently. It backs
// TestAttachThreadCmdActivatesBeforeAttaching and
// TestHandleThreadAttachedReportsActivateErrorButStillOpensThread.
type activateThenAttachWorkspace struct {
	rootTestWorkspace
	calls []string

	activateErr error
	attachWS    workspace.Workspace
	attachErr   error
}

func (w *activateThenAttachWorkspace) ActivateThread(context.Context, string) (proto.Thread, error) {
	w.calls = append(w.calls, "activate")
	return proto.Thread{}, w.activateErr
}

func (w *activateThenAttachWorkspace) AttachThread(context.Context, string) (workspace.Workspace, func(), error) {
	w.calls = append(w.calls, "attach")
	return w.attachWS, func() {}, w.attachErr
}

// TestAttachThreadCmdActivatesBeforeAttaching proves attachThreadCmd calls
// ActivateThread before AttachThread — this is the drill-in path's
// deliberate revival of a thread that may not currently be running, as
// opposed to the dock's background refresh (dispatchThreadActivityRefresh),
// which must call AttachThread only and never trigger a spawn.
func TestAttachThreadCmdActivatesBeforeAttaching(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	ws := &activateThenAttachWorkspace{attachWS: &rootTestWorkspace{}}
	r.com.Workspace = ws

	cmd := r.attachThreadCmd("thread-1", "session-1", "thread")
	require.NotNil(t, cmd)
	msg, ok := cmd().(threadAttachedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	require.NoError(t, msg.activateErr)
	require.Equal(t, []string{"activate", "attach"}, ws.calls,
		"attachThreadCmd must activate the thread before attaching to it")
}

// TestHandleThreadAttachedWarnsOnActivateErrorButStillOpensThread proves
// that an ActivateThread failure does not abort the attach: the thread
// still opens (read-only, via AttachThread's own fallback), and the
// activation failure is explained to the user as a warning (via
// util.ReportWarn), not flagged as an error — the common case is a
// merged/merging thread, for which read-only is the correct and permanent
// state, not a failure of anything the user did. The reason text itself
// must still reach the user rather than being swallowed — see
// AppWorkspace.AttachThread and attachThreadCmd's doc comments for why it
// would otherwise only surface once the person tries to type into a
// read-only thread.
func TestHandleThreadAttachedWarnsOnActivateErrorButStillOpensThread(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	r.attachment.pendingID = "thread-1"

	activateErr := fmt.Errorf("thread could not be revived")
	model, cmd := r.handleThreadAttached(threadAttachedMsg{
		id:          "thread-1",
		sessionID:   "session-1",
		name:        "thread",
		ws:          &rootTestWorkspace{},
		detach:      func() {},
		activateErr: activateErr,
	})
	r = model.(*Root)

	// The thread still opened despite the activation failure.
	require.Equal(t, screenThread, r.active)
	require.NotNil(t, r.attachment.thread)

	// The activation failure was explained as a warning, not swallowed
	// and not flagged as an error.
	var reported *util.InfoMsg
	var walk func(c tea.Cmd)
	walk = func(c tea.Cmd) {
		if c == nil {
			return
		}
		switch m := safeRunCmd(c).(type) {
		case tea.BatchMsg:
			for _, cc := range m {
				walk(cc)
			}
		case util.InfoMsg:
			if m.Type == util.InfoTypeWarn {
				mm := m
				reported = &mm
			}
		}
	}
	walk(cmd)
	require.NotNil(t, reported, "the ActivateThread failure must be explained to the user, not swallowed")
	require.Equal(t, util.InfoTypeWarn, reported.Type, "a thread that could not be revived is a state to explain, not an error to flag")
	require.Contains(t, reported.Msg, activateErr.Error(), "the underlying reason must still reach the user")
}
