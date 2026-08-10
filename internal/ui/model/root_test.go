package model

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/workspace"
	"github.com/stretchr/testify/require"
)

// rootTestWorkspace is a minimal workspace.Workspace stub sized for driving
// Root/New end to end: unlike strandsTestWorkspace (which only exercises
// the cache's ListStrands round trip), New() also touches
// PermissionSkipRequests/AgentIsReady/Config on construction, so those need
// stubbing here too.
type rootTestWorkspace struct {
	workspace.Workspace
	supportsStrands bool
}

func (w *rootTestWorkspace) Config() *config.Config {
	providers := csync.NewMap[string, config.ProviderConfig]()
	// New() must land in uiLanding/uiFocusEditor (not uiOnboarding) so
	// handleGlobalKeys — and therefore the strands key — is reachable; that
	// needs at least one enabled provider (see Config.IsConfigured).
	providers.Set("test-provider", config.ProviderConfig{ID: "test-provider"})
	return &config.Config{
		Providers: providers,
		Options:   &config.Options{TUI: &config.TUIOptions{}},
	}
}

func (w *rootTestWorkspace) PermissionSkipRequests() bool              { return false }
func (w *rootTestWorkspace) AgentIsReady() bool                        { return false }
func (w *rootTestWorkspace) SupportsStrands() bool                     { return w.supportsStrands }
func (w *rootTestWorkspace) WorkingDir() string                        { return "/tmp" }
func (w *rootTestWorkspace) ProjectNeedsInitialization() (bool, error) { return false, nil }

func (w *rootTestWorkspace) ListStrands(context.Context) ([]proto.Strand, error) {
	return nil, nil
}

// newTestRoot builds a Root over rootTestWorkspace, configured so New()
// lands in uiLanding/uiFocusEditor where the global key switch (and
// therefore the strands key) is reachable.
func newTestRoot(t *testing.T, supportsStrands bool) *Root {
	t.Helper()
	ws := &rootTestWorkspace{supportsStrands: supportsStrands}
	com := common.DefaultCommon(context.Background(), ws)
	return NewRoot(com, "", false)
}

func ctrlE() tea.KeyPressMsg {
	return tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'e', Text: ""}
}

// drainShowDashboard runs cmd the way the Bubble Tea runtime would (unwrapping
// tea.BatchMsg) until it finds the showStrandsDashboardMsg that
// UI.handleGlobalKeys produces for the strands key, feeding it back into
// r.Update. Other leaf messages are executed for side effects and dropped,
// mirroring runCmds in session_busy_test.go.
func drainShowDashboard(t *testing.T, r *Root, cmd tea.Cmd) *Root {
	t.Helper()
	if cmd == nil {
		return r
	}
	// Alongside the strands key, handleGlobalKeys' surrounding Update pass
	// also kicks off unrelated background refreshes (LSP, busy state, ...)
	// that rootTestWorkspace's minimal stub doesn't implement. Those leaves
	// aren't what this test is after, so a panic from one is swallowed —
	// only the showStrandsDashboardMsg branch matters here.
	msg := safeRunCmd(cmd)
	switch msg := msg.(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			r = drainShowDashboard(t, r, c)
		}
		return r
	case showStrandsDashboardMsg:
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

// TestStrandsKeyIgnoredWhenUnsupported pins that pressing the strands
// toggle never switches screens when the workspace doesn't support
// strands: the check lives inside UI.handleGlobalKeys, reached because
// screenMain forwards keys straight to r.main.
func TestStrandsKeyIgnoredWhenUnsupported(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, false)
	_, cmd := r.Update(ctrlE())
	// drainShowDashboard only switches screens on showStrandsDashboardMsg;
	// with SupportsStrands() false, UI.handleGlobalKeys reports an info
	// message instead (see the ui.go Strands case), so this must be a
	// no-op regardless of whatever leaf messages the cmd tree contains.
	r = drainShowDashboard(t, r, cmd)
	require.Equal(t, screenMain, r.active)
}

// TestStrandsKeyTogglesDashboard drives ctrl+e main -> dashboard -> main.
func TestStrandsKeyTogglesDashboard(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)

	// main -> dashboard: ctrl+e is handled by UI.handleGlobalKeys, which
	// returns a cmd carrying showStrandsDashboardMsg; Root must apply it.
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
	r.dashboard = newStrandsDashboard(r.com)

	model, _ := r.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	r = model.(*Root)

	require.Equal(t, 120, r.width)
	require.Equal(t, 40, r.height)
	require.Equal(t, 120, r.main.width)
	require.Equal(t, 120, r.dashboard.width)
	require.Equal(t, 40, r.dashboard.height)
}

// TestStrandEventMsgDroppedWhenNotAttached exercises both "no strand
// attached" and "different strand attached" cases; neither should panic or
// misroute.
func TestStrandEventMsgDroppedWhenNotAttached(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)

	require.NotPanics(t, func() {
		_, cmd := r.Update(strandEventMsg{strandID: "s1", inner: tea.WindowSizeMsg{}})
		require.Nil(t, cmd)
	})

	strandUI := New(common.DefaultCommon(context.Background(), &rootTestWorkspace{}), "", false, WithEmbedded())
	r.strand = &strandAttachment{strandID: "s1", ui: strandUI}
	r.active = screenStrand

	require.NotPanics(t, func() {
		_, cmd := r.Update(strandEventMsg{strandID: "s2", inner: tea.WindowSizeMsg{}})
		require.Nil(t, cmd)
	})
}

// TestStrandEventMsgReachesAttachedStrand confirms a tagged event matching
// the currently attached strand is forwarded into that strand's own *UI.
func TestStrandEventMsgReachesAttachedStrand(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	strandUI := New(common.DefaultCommon(context.Background(), &rootTestWorkspace{}), "", false, WithEmbedded())
	r.strand = &strandAttachment{strandID: "s1", ui: strandUI}
	r.active = screenStrand

	_, cmd := r.Update(strandEventMsg{strandID: "s1", inner: tea.WindowSizeMsg{Width: 80, Height: 24}})
	if cmd != nil {
		cmd()
	}

	require.Equal(t, 80, strandUI.width)
	require.Equal(t, 24, strandUI.height)
}
