package model

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/common"
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

func (w *rootTestWorkspace) PermissionSkipRequests() bool              { return false }
func (w *rootTestWorkspace) AgentIsReady() bool                        { return false }
func (w *rootTestWorkspace) SupportsThreads() bool                     { return w.supportsThreads }
func (w *rootTestWorkspace) WorkingDir() string                        { return "/tmp" }
func (w *rootTestWorkspace) ProjectNeedsInitialization() (bool, error) { return false, nil }

func (w *rootTestWorkspace) ListThreads(context.Context) ([]proto.Thread, error) {
	w.listThreadsCalls++
	return nil, nil
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
	r.dashboard = newThreadsDashboard(r.com, &r.main.threadList)

	model, _ := r.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	r = model.(*Root)

	require.Equal(t, 120, r.width)
	require.Equal(t, 40, r.height)
	require.Equal(t, 120, r.main.lay.width)
	require.Equal(t, 120, r.dashboard.width)
	require.Equal(t, 40, r.dashboard.height)
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
	r.thread = &threadAttachment{threadID: "s1", ui: threadUI}
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
	r.thread = &threadAttachment{threadID: "s1", ui: threadUI}
	r.active = screenThread

	_, cmd := r.Update(threadEventMsg{threadID: "s1", inner: tea.WindowSizeMsg{Width: 80, Height: 24}})
	if cmd != nil {
		cmd()
	}

	require.Equal(t, 80, threadUI.lay.width)
	require.Equal(t, 24, threadUI.lay.height)
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
	r.thread = &threadAttachment{threadID: "s1", ui: threadUI}
	r.active = screenThread

	model, cmd := r.Update(tea.KeyPressMsg{Mod: tea.ModAlt, Code: tea.KeyUp})
	r = model.(*Root)
	if cmd != nil {
		cmd()
	}

	require.Equal(t, screenMain, r.active)
	require.Nil(t, r.thread, "leaveThreadToMain must tear down the attachment")
}

// TestLeaveThreadToMain covers the method directly: it tears down the
// attachment (stop/detach both run) and lands on screenMain, not
// screenDashboard the way leaveThread does.
func TestLeaveThreadToMain(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	threadUI := New(common.DefaultCommon(context.Background(), &rootTestWorkspace{}), "", false, WithEmbedded())

	stopped, detached := false, false
	r.thread = &threadAttachment{
		threadID: "s1",
		ui:       threadUI,
		stop:     func() { stopped = true },
		detach:   func() { detached = true },
	}
	r.active = screenThread

	cmd := r.leaveThreadToMain()
	require.Equal(t, screenMain, r.active)
	require.Nil(t, r.thread)
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
	r.thread = &threadAttachment{
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
	require.Nil(t, r.thread)

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
