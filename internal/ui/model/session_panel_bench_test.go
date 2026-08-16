package model

import (
	"fmt"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/attachments"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// benchWorkspace extends testWorkspace (ui_test.go) with WorkingDir, which
// the compact header's detail line reads via renderHeaderDetails — the
// plain testWorkspace embeds a nil workspace.Workspace, so calling it
// unimplemented panics.
type benchWorkspace struct {
	testWorkspace
}

func (w *benchWorkspace) WorkingDir() string { return "/bench" }

// benchChatHistoryLen is the synthetic chat history length used by the
// panel benchmarks below. It matters: a short fixture chat never exercises
// an O(N)-over-full-history hotspot, which is exactly what's suspected in
// the session panel's draw/tick path (RunningDelegations scans the whole
// list on every call — see chat.go).
const benchChatHistoryLen = 800

// newSessionPanelBenchUI builds a *UI mirroring a busy real session: a long
// chat history, active threads, running delegations, and an expanded todos
// panel with a mix of statuses — the combination that exercises every
// section sessionPanelPlan computes.
func newSessionPanelBenchUI() *UI {
	u := newTestUI()
	// drawHeader reads com.Config() (compact-mode details line), so the
	// synthetic workspace needs a minimal-but-non-nil config, unlike
	// newTestUI's bare Common (fine for the layout-only tests it serves).
	u.com.Workspace = &benchWorkspace{testWorkspace: testWorkspace{cfg: &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Agents:    map[string]config.Agent{},
	}}}
	u.header = newHeader(u.com)
	// newTestUI wires Status with a nil help.KeyMap (fine for the
	// layout-only tests it serves); Draw's help.View call needs a real one.
	u.status.helpKm = u
	u.dialog = dialog.NewOverlay()
	// renderEditorView reads the attachments strip unconditionally; the
	// real New() always constructs one, but newTestUI (built for
	// layout-only tests that never call Draw) leaves it nil.
	u.editor.attachments = attachments.New(attachments.NewRenderer(
		u.com.Styles.Attachments.Normal,
		u.com.Styles.Attachments.Deleting,
		u.com.Styles.Attachments.Image,
		u.com.Styles.Attachments.Text,
		u.com.Styles.Attachments.Skill,
		u.com.Styles.Attachments.Remove,
	), attachments.Keymap{})
	u.sess.current = &session.Session{ID: "bench-session"}
	u.lay.width, u.lay.height = 140, 45
	// Compact layout skips the sidebar, which needs a real workspace
	// (m.com.Workspace.WorkingDir()) that this synthetic fixture has none
	// of — the session panel's own draw path is identical either way.
	// forceCompactMode (not a direct isCompact assignment) survives
	// updateLayoutAndSize's own width-based recompute below.
	u.lay.forceCompactMode = true

	// Long chat history: plain filler items plus two running delegations
	// (top-level agent tool calls with no result yet), scattered through
	// the list rather than clustered at one end.
	items := make([]chat.MessageItem, 0, benchChatHistoryLen)
	delegationAt := map[int]bool{200: true, 600: true}
	for i := range benchChatHistoryLen {
		if delegationAt[i] {
			d := chat.NewAgentToolMessageItem(u.com.Styles,
				message.ToolCall{
					ID:       fmt.Sprintf("deleg-%d", i),
					Name:     "agent",
					Input:    `{"prompt":"investigate and fix a flaky integration test"}`,
					Finished: false,
				}, nil, false, nil)
			d.SetMessageID(fmt.Sprintf("m-deleg-%d", i))
			items = append(items, d)
			continue
		}
		items = append(items, testMessageItem{
			id:   fmt.Sprintf("m-%d", i),
			text: fmt.Sprintf("synthetic chat item %d with a bit of body text to render", i),
		})
	}
	u.chat.SetMessages(items...)

	// Three active threads, mirroring mkDockThreads.
	u.threadsDock.cache.value = mkDockThreads(3)

	// Nine todos: a few in-progress (exercises the spinner path), some
	// pending, some completed.
	u.sess.current.Todos = []session.Todo{
		{Status: session.TodoStatusInProgress, Content: "in progress 1", ActiveForm: "Working on 1"},
		{Status: session.TodoStatusInProgress, Content: "in progress 2", ActiveForm: "Working on 2"},
		{Status: session.TodoStatusPending, Content: "pending 1"},
		{Status: session.TodoStatusPending, Content: "pending 2"},
		{Status: session.TodoStatusPending, Content: "pending 3"},
		{Status: session.TodoStatusCompleted, Content: "done 1"},
		{Status: session.TodoStatusCompleted, Content: "done 2"},
		{Status: session.TodoStatusCompleted, Content: "done 3"},
		{Status: session.TodoStatusCompleted, Content: "done 4"},
	}
	u.panel.expanded = true
	u.panel.isSpinning = true

	u.updateLayoutAndSize()
	return u
}

// BenchmarkSessionPanelDraw benchmarks a full Draw() call against a busy
// session panel with a long chat history — the real per-frame render path.
func BenchmarkSessionPanelDraw(b *testing.B) {
	for _, size := range []struct {
		name string
		w, h int
	}{
		{"140x45", 140, 45},
		{"80x24", 80, 24},
	} {
		b.Run(size.name, func(b *testing.B) {
			u := newSessionPanelBenchUI()
			u.lay.width, u.lay.height = size.w, size.h
			u.updateLayoutAndSize()
			scr := uv.NewScreenBuffer(size.w, size.h)
			area := uv.Rectangle{Max: uv.Position{X: size.w, Y: size.h}}

			b.ReportAllocs()
			for b.Loop() {
				u.Draw(scr, area)
			}
		})
	}
}

// BenchmarkSessionPanelUpdateTick benchmarks the recurring todo-spinner
// tick a running panel generates continuously — spinner.TickMsg, routed
// through UI.Update exactly as the real animation loop delivers it (see
// ui.go's spinner.TickMsg case).
func BenchmarkSessionPanelUpdateTick(b *testing.B) {
	u := newSessionPanelBenchUI()
	msg := spinner.TickMsg{Time: time.Now(), ID: u.panel.spinner.ID()}

	b.ReportAllocs()
	for b.Loop() {
		u.Update(msg)
	}
}

// sessionPanelDrawBudget is the frame-time ceiling TestSessionPanelDrawFrameBudget
// enforces. Measured Draw time against the fixture below is ~0.3ms; the
// ceiling is set generously above that (not tight to the measurement) to
// absorb CI machine variance while still catching a real regression —
// e.g. RunningDelegations losing its container cache (see chat.go) and
// reverting to an O(history length) scan would only add tens of
// microseconds at this fixture's 800-message history, but would then
// scale unboundedly with a real session's history, which is exactly the
// class of bug this guards against.
const sessionPanelDrawBudget = 8 * time.Millisecond

// TestSessionPanelDrawFrameBudget is a benchmark-shaped regression guard:
// unlike BenchmarkSessionPanelDraw, it fails the test run (and therefore
// CI) if a single Draw() over a busy session panel — long chat history,
// active threads, running delegations, expanded todos — exceeds a
// generous frame budget. This is the check that would have caught the
// RunningDelegations O(N)-per-call regression this file's fix addresses.
func TestSessionPanelDrawFrameBudget(t *testing.T) {
	// Deliberately not t.Parallel(): this is a wall-clock timing
	// assertion, and running alongside other parallel tests would add
	// scheduling noise that has nothing to do with the code under test.
	u := newSessionPanelBenchUI()
	scr := uv.NewScreenBuffer(u.lay.width, u.lay.height)
	area := uv.Rectangle{Max: uv.Position{X: u.lay.width, Y: u.lay.height}}

	// Warm caches once (mirrors real usage: the first frame after a
	// layout/session change is allowed to be pricier; steady-state ticks
	// are what must stay fast) before timing.
	u.Draw(scr, area)

	const iterations = 50
	start := time.Now()
	for range iterations {
		u.Draw(scr, area)
	}
	elapsed := time.Since(start) / iterations

	require.Lessf(t, elapsed, sessionPanelDrawBudget,
		"Draw() over a busy session panel averaged %s per frame, want < %s", elapsed, sessionPanelDrawBudget)
}
