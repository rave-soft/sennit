package dialog

import (
	"context"
	"errors"
	"image"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/stats"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// statsTestWorkspace records what scope the dialog asked for and returns
// a canned snapshot, so these tests cover the screen's own behavior
// (which scope each tab requests, what it renders, what it caches) rather
// than the aggregation, which internal/stats covers.
type statsTestWorkspace struct {
	workspace.Workspace
	requested []stats.Request
	snap      stats.Snapshot
	err       error
}

// KnownProviders: no test here renders a provider list.
func (w statsTestWorkspace) KnownProviders() []catwalk.Provider { return nil }

// SkillStates, BuiltinSkills: the skills panel reads these; no test
// here has a catalog beyond what the binary ships.
func (w statsTestWorkspace) SkillStates() []*skills.SkillState { return nil }
func (w statsTestWorkspace) ConfigProblems() []config.Problem  { return nil }
func (w statsTestWorkspace) BuiltinSkills() []*skills.Skill    { return skills.DiscoverBuiltin() }

func (w *statsTestWorkspace) WorkingDir() string { return "/repo" }

func (w *statsTestWorkspace) Stats(_ context.Context, req stats.Request) (stats.Snapshot, error) {
	w.requested = append(w.requested, req)
	return w.snap, w.err
}

func newStatsTestDialog(t *testing.T, ws *statsTestWorkspace, sessionID string) *Stats {
	t.Helper()
	s := styles.SennitDark()
	return NewStats(&common.Common{Styles: &s, Workspace: ws}, sessionID)
}

// sampleSnapshot is a snapshot with one of everything the screen draws.
func sampleSnapshot() stats.Snapshot {
	return stats.Snapshot{
		Totals: stats.Project{Sessions: 12, PromptTokens: 412_480, CompletionTokens: 38_120, Cost: 1.24, TimeSeconds: 3840},
		Models: []stats.Model{
			{Model: "cx/gpt-5.6-sol", Provider: "omniroute", PromptTokens: 300_000, CompletionTokens: 20_000, Cost: 1.0, TimeSeconds: 2400, MessageCount: 120, Delegations: 3, Succeeded: 2},
			{Model: "qwen36-local", Provider: "local", PromptTokens: 112_480, CompletionTokens: 18_120, TimeSeconds: 1440, MessageCount: 40, Approximate: true},
		},
		Agents: []stats.Agent{
			{Name: "developer-junior", Runs: 6, PromptTokens: 220_000, CompletionTokens: 20_000, Cost: 0.8, TimeSeconds: 2000, Delegations: 2, Succeeded: 1},
			{Name: "reviewer", Runs: 4, PromptTokens: 90_000, CompletionTokens: 9_000, Cost: 0.3, TimeSeconds: 900},
		},
		Outcome: stats.Outcome{Total: 3, Landed: 2, Failed: 1},
	}
}

func drawStats(t *testing.T, d *Stats) string {
	t.Helper()
	area := image.Rect(0, 0, 100, 40)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	d.Draw(scr, area)
	return scr.Render()
}

// The screen opens on the project scope: a session's own usage is already
// on the sidebar, so the project is the first thing this screen adds.
func TestStats_OpensOnProjectScope(t *testing.T) {
	t.Parallel()

	ws := &statsTestWorkspace{snap: sampleSnapshot()}
	d := newStatsTestDialog(t, ws, "sess-1")

	cmd := d.LoadCmd()
	require.NotNil(t, cmd)
	cmd()

	require.Len(t, ws.requested, 1)
	require.Equal(t, stats.ScopeProject, ws.requested[0].Scope)
	require.Equal(t, "/repo", ws.requested[0].ProjectPath)
}

// Each tab loads its own scope, and only once: coming back to a tab
// already loaded must not re-query.
func TestStats_TabsLoadTheirScopeOnce(t *testing.T) {
	t.Parallel()

	ws := &statsTestWorkspace{snap: sampleSnapshot()}
	d := newStatsTestDialog(t, ws, "sess-1")
	deliver := func(cmd tea.Cmd) {
		require.NotNil(t, cmd)
		d.HandleMsg(cmd())
	}

	deliver(d.LoadCmd()) // project

	// Forward to All projects.
	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	cmdAction, ok := action.(ActionCmd)
	require.True(t, ok, "switching to an unloaded tab must ask for its data")
	deliver(cmdAction.Cmd)
	require.Equal(t, stats.ScopeGlobal, ws.requested[1].Scope)

	// Forward again wraps to Session.
	action = d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	cmdAction, ok = action.(ActionCmd)
	require.True(t, ok)
	deliver(cmdAction.Cmd)
	require.Equal(t, stats.ScopeSession, ws.requested[2].Scope)
	require.Equal(t, "sess-1", ws.requested[2].SessionID)

	// Back to a loaded tab: no new query.
	before := len(ws.requested)
	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab}))
	require.Len(t, ws.requested, before, "an already-loaded tab must not re-query")
}

// With no session yet, the session tab says so instead of querying for a
// session that does not exist.
func TestStats_SessionTabWithoutASession(t *testing.T) {
	t.Parallel()

	ws := &statsTestWorkspace{snap: sampleSnapshot()}
	d := newStatsTestDialog(t, ws, "")
	d.active = 0

	require.Nil(t, d.loadTab(0), "no session means nothing to ask for")
	require.Contains(t, drawStats(t, d), "No active session")
}

// The rendered screen has to carry the two caveats with it: a "~" row is
// an estimate, and "landed" is a delegation status, not a review verdict.
// Without those, both numbers read as more certain than they are.
func TestStats_RendersBreakdownsAndCaveats(t *testing.T) {
	t.Parallel()

	ws := &statsTestWorkspace{snap: sampleSnapshot()}
	d := newStatsTestDialog(t, ws, "sess-1")
	d.HandleMsg(d.LoadCmd()())

	out := drawStats(t, d)
	require.Contains(t, out, "By model")
	require.Contains(t, out, "By subagent")
	require.Contains(t, out, "developer-junior")
	require.Contains(t, out, "2 of 3 delegations")
	require.Contains(t, out, "~", "an approximate row must be marked")
	require.Contains(t, out, "review verdicts are not recorded")
}

// A failed query is reported on the tab, not swallowed into an empty
// screen that reads as "you have used nothing".
func TestStats_ReportsQueryFailure(t *testing.T) {
	t.Parallel()

	ws := &statsTestWorkspace{err: errors.New("database is locked")}
	d := newStatsTestDialog(t, ws, "sess-1")
	d.HandleMsg(d.LoadCmd()())

	require.Contains(t, drawStats(t, d), "database is locked")
}

// An empty scope reads as "nothing recorded", which is a different
// statement from an error and from a zero-cost project.
func TestStats_EmptyScopeSaysSo(t *testing.T) {
	t.Parallel()

	ws := &statsTestWorkspace{}
	d := newStatsTestDialog(t, ws, "sess-1")
	d.HandleMsg(d.LoadCmd()())

	require.Contains(t, drawStats(t, d), "Nothing recorded")
}

// statsBar always shows something for a non-zero row: rounding a real
// value down to an empty bar would read as "no usage here".
func TestStatsBar_NonZeroAlwaysVisible(t *testing.T) {
	t.Parallel()

	require.Equal(t, strings.Repeat("·", statsBarWidth), statsBar(0, 100))
	require.Contains(t, statsBar(1, 1_000_000), "█")
	require.Equal(t, strings.Repeat("█", statsBarWidth), statsBar(100, 100))
}

// A transient failure must not stick: once a retry of the same tab
// succeeds, the old error must not still be shown alongside (or instead
// of) the fresh data.
func TestStats_SuccessfulReloadClearsEarlierError(t *testing.T) {
	t.Parallel()

	ws := &statsTestWorkspace{err: errors.New("database is locked")}
	d := newStatsTestDialog(t, ws, "sess-1")
	d.HandleMsg(d.LoadCmd()())
	require.Contains(t, drawStats(t, d), "database is locked")

	// Simulate switching away and back: loadTab only re-queries because
	// the failed attempt never marked the tab loaded.
	ws.err = nil
	ws.snap = sampleSnapshot()
	cmd := d.loadTab(d.active)
	require.NotNil(t, cmd, "a tab that failed to load must be retried")
	d.HandleMsg(cmd())

	out := drawStats(t, d)
	require.NotContains(t, out, "database is locked", "a successful reload must clear the earlier error")
	require.Contains(t, out, "By model")
}

// statsFrontDialog stands in for whatever might open over Stats while its
// queries are in flight — a permission prompt raised by the sweep itself
// is the realistic case. It never handles StatsLoadedMsg, so if the
// message were routed to whichever dialog is on top instead of by
// address, it would be silently dropped here.
type statsFrontDialog struct{}

func (statsFrontDialog) ID() string                               { return "stats-front-test" }
func (statsFrontDialog) HandleMsg(tea.Msg) Action                 { return nil }
func (statsFrontDialog) Draw(uv.Screen, uv.Rectangle) *tea.Cursor { return nil }

var _ Dialog = statsFrontDialog{}

// A load result must reach Stats even when another dialog has opened on
// top of it before the result arrives, so the tab does not get stuck on
// "Reading usage…" forever.
func TestStats_LoadedMsgReachesDialogBehindAnotherOne(t *testing.T) {
	t.Parallel()

	ws := &statsTestWorkspace{snap: sampleSnapshot()}
	d := newStatsTestDialog(t, ws, "sess-1")
	cmd := d.LoadCmd()
	require.NotNil(t, cmd)

	var overlay Overlay
	overlay.OpenDialog(d)
	overlay.OpenDialog(statsFrontDialog{})

	overlay.Update(cmd())

	out := drawStats(t, d)
	require.Contains(t, out, "By model", "the result must reach Stats even though another dialog is in front")
}

// TestStats_IgnoresLoadedMsgFromADifferentInstance is the regression test
// for a stale sweep landing in the wrong dialog instance: DialogID routes
// StatsLoadedMsg by the constant StatsID, not by instance, so closing this
// dialog and reopening a fresh one — for a different session, the common
// case — while the old instance's sweep is still in flight (up to 10s for
// the global scope) would otherwise let that late result be shown as the
// new instance's own.
func TestStats_IgnoresLoadedMsgFromADifferentInstance(t *testing.T) {
	t.Parallel()

	ws := &statsTestWorkspace{snap: sampleSnapshot()}
	d := newStatsTestDialog(t, ws, "sess-2")

	// A result from an abandoned instance opened for a different session.
	action := d.HandleMsg(StatsLoadedMsg{
		Scope:     stats.ScopeProject,
		Snapshot:  sampleSnapshot(),
		SessionID: "sess-1",
	})

	require.Nil(t, action)
	require.False(t, d.tabs[1].loaded, "a result addressed to a different instance must not populate this one's tab")

	out := drawStats(t, d)
	require.NotContains(t, out, "By model", "the stale snapshot must not be shown as this instance's data")
}

// The same model id served by two providers is two rows with two costs.
// Naming only the model would print what looks like the same row twice,
// so the provider is appended exactly where it tells them apart.
func TestStats_DisambiguatesSameModelFromTwoProviders(t *testing.T) {
	t.Parallel()

	snap := sampleSnapshot()
	snap.Models = []stats.Model{
		{Model: "gpt-5.6", Provider: "omniroute", PromptTokens: 100},
		{Model: "gpt-5.6", Provider: "openai", PromptTokens: 50},
		{Model: "qwen36-local", Provider: "local", PromptTokens: 10},
	}
	ws := &statsTestWorkspace{snap: snap}
	d := newStatsTestDialog(t, ws, "sess-1")
	d.HandleMsg(d.LoadCmd()())

	out := drawStats(t, d)
	require.Contains(t, out, "gpt-5.6 (omniroute)")
	require.Contains(t, out, "gpt-5.6 (openai)")
	require.NotContains(t, out, "qwen36-local (local)",
		"a model served by only one provider needs no disambiguation")
}
