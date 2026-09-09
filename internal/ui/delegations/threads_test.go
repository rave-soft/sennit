package delegations

import (
	"image"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func testStyles() *styles.Styles {
	s := styles.SennitDark()
	return &s
}

func newTestThreadsDashboard(t *testing.T, ws *threadsTestWorkspace) *Dashboard {
	t.Helper()
	com := &common.Common{Workspace: ws, Styles: testStyles()}
	m := New(com, &ListCache{})
	m.SetSize(80, 20)
	return m
}

func TestThreadItemRenderRespectsWidth(t *testing.T) {
	t.Parallel()

	item := newThreadItem(testStyles(), proto.Thread{
		ID:     "s1",
		Name:   "add-auth",
		Status: "running",
		Branch: "thread/add-auth",
		Goal:   "Implement OAuth login end to end across every service",
	}, computeThreadsColumns(120))

	for _, width := range []int{40, 120} {
		require.NotPanics(t, func() {
			rendered := item.Render(width)
			require.LessOrEqual(t, ansi.StringWidth(rendered), width)
		})
	}
}

func TestThreadItemRenderTruncatesGoal(t *testing.T) {
	t.Parallel()

	longGoal := strings.Repeat("implement a very long goal description ", 10)
	item := newThreadItem(testStyles(), proto.Thread{
		ID:     "s1",
		Name:   "add-auth",
		Status: "running",
		Branch: "thread/add-auth",
		Goal:   longGoal,
	}, computeThreadsColumns(60))

	rendered := item.Render(60)
	require.LessOrEqual(t, ansi.StringWidth(rendered), 60)
	require.Contains(t, ansi.Strip(rendered), "…", "goal should be truncated with an ellipsis marker")
}

func TestThreadStatusStyleIdleIsNeitherDoneNorError(t *testing.T) {
	t.Parallel()

	sty := testStyles()
	idle := threadStatusStyle(sty, "idle")
	require.Equal(t, sty.Threads.StatusIdle, idle)
	require.NotEqual(t, threadStatusStyle(sty, "completed"), idle)
	require.NotEqual(t, threadStatusStyle(sty, "failed"), idle)
}

// TestThreadStatusStyleClasses pins each status onto its color class — the
// dashboard is scanned by color before it is read.
func TestThreadsDashboardRebuildItemsResizesListForDetailPane(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws) // SetSize(80, 20) while the cache is still empty.
	require.Nil(t, m.selected())
	beforeHeight := m.list.Height()

	m.cache.Cache.Value = []proto.Thread{{ID: "s1", Name: "one", Status: "running"}}
	m.RebuildItems()

	require.NotNil(t, m.selected(), "RebuildItems lands on the first row when nothing was selected")
	wantHeight := max(0, m.height-m.chromeHeight())
	require.Equal(t, wantHeight, m.list.Height(),
		"the list must be sized off the chrome height of the now-selected row")
	require.Less(t, wantHeight, beforeHeight,
		"the detail pane must shrink the list once a row is selected")
}

func TestThreadsDashboardHandleKeyEnter(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.cache.Cache.Value = []proto.Thread{{ID: "s1", Name: "one"}}
	m.RebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.True(t, handled)
	require.NotNil(t, cmd)
	msg, ok := cmd().(EnterMsg)
	require.True(t, ok)
	require.Equal(t, "s1", msg.ID)
}

func TestThreadsDashboardHandleKeyRemove(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.cache.Cache.Value = []proto.Thread{{ID: "s1", Kind: string(proto.ThreadKindThread)}}
	m.RebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "x", Code: 'x'})
	require.True(t, handled)
	require.NotNil(t, cmd)
	msg, ok := cmd().(ConfirmRemoveMsg)
	require.True(t, ok, "x should request confirmation, not remove directly")
	require.Equal(t, "s1", msg.ID)
}

// TestThreadsDashboardTaskCannotOpenOrCleanup pins the ordinary-task action
// boundary: tasks can be cancelled while active but cannot be opened or
// cleaned up from this dashboard.
func TestThreadsDashboardTaskCannotOpenOrCleanup(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.cache.Cache.Value = []proto.Thread{{ID: "t1", Kind: "task", Status: "running"}}
	m.RebuildItems()
	m.list.SelectFirst()

	require.Nil(t, m.runAction(actionOpen))
	require.Nil(t, m.runAction(actionCleanup))
}

func TestThreadsDashboardHandleKeyCancelTask(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.cache.Cache.Value = []proto.Thread{{ID: "t1", Kind: "task", Status: "running"}}
	m.RebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "c", Code: 'c'})
	require.True(t, handled)
	require.NotNil(t, cmd)
	msg, ok := cmd().(CancelDelegationMsg)
	require.True(t, ok)
	require.Equal(t, "t1", msg.ID)
	require.Equal(t, "task", msg.Kind)
}

// TestThreadsDashboardHandleKeyCancelThread proves the cancel key also
// emits CancelDelegationMsg for a non-terminal thread row: unlike a task,
// cancelling a thread leaves its worktree and branch on disk rather than
// tearing it down (see Manager.Cancel), so there is no reason to withhold
// the key from a thread row the way an earlier step did.
func TestThreadsDashboardHandleKeyCancelThread(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.cache.Cache.Value = []proto.Thread{{ID: "s1", Kind: "thread", Status: "running"}}
	m.RebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "c", Code: 'c'})
	require.True(t, handled)
	require.NotNil(t, cmd)
	msg, ok := cmd().(CancelDelegationMsg)
	require.True(t, ok)
	require.Equal(t, "s1", msg.ID)
	require.Equal(t, "thread", msg.Kind)
}

// TestThreadsDashboardHandleKeyCancelSkipsTerminalTask proves cancel is a
// no-op for a task that's already reached a terminal status.
func TestThreadsDashboardHandleKeyCancelSkipsTerminalTask(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.cache.Cache.Value = []proto.Thread{{ID: "t1", Kind: "task", Status: "completed"}}
	m.RebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "c", Code: 'c'})
	require.True(t, handled)
	require.Nil(t, cmd, "an already-terminal task should not re-trigger a cancel")
}

func TestThreadsDashboardHandleKeyReload(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.cache.Cache.Timestamp = time.Now() // fresh cache would normally skip a refresh

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "r", Code: 'r'})
	require.True(t, handled)
	require.NotNil(t, cmd, "'r' should force a refresh even when the cache is fresh")
}

func TestThreadsDashboardHandleKeyUnrecognized(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "z", Code: 'z'})
	require.False(t, handled)
	require.Nil(t, cmd)
}

func TestThreadsDashboardSetActiveDispatchesRefreshWhenStale(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)

	cmd := m.SetActive(true)
	require.NotNil(t, cmd, "an empty/stale cache should trigger a refresh on activation")
	require.True(t, m.active)
}

func TestThreadsDashboardApplyThreadsLoadedRebuildsItems(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)

	threads := []proto.Thread{{ID: "s1", Name: "one"}, {ID: "s2", Name: "two"}}
	cmds := m.ApplyThreadsLoaded(LoadedMsg{Gen: m.cache.Cache.Generation, Threads: threads})
	require.Nil(t, cmds)
	require.Equal(t, 2, m.list.Len())
}

func TestThreadsDashboardApplyThreadEventRebuildsItems(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.active = true

	cmd := m.ApplyThreadEvent(pubsub.Event[proto.Thread]{
		Type:    pubsub.CreatedEvent,
		Payload: proto.Thread{ID: "s1", Name: "one"},
	})
	require.Equal(t, 1, m.list.Len())
	require.NotNil(t, cmd, "active dashboard should re-arm a refresh after the event invalidates the TTL")
}

// dashboardWith builds a dashboard sized 120x30 over the given threads,
// drawn once so its hit zones exist — clicking is only meaningful against
// a frame that was actually painted.
func dashboardWith(t *testing.T, threads ...proto.Thread) *Dashboard {
	t.Helper()
	m := newTestThreadsDashboard(t, &threadsTestWorkspace{supported: true})
	m.SetSize(120, 30)
	m.cache.Cache.Value = threads
	m.RebuildItems()
	scr := uv.NewScreenBuffer(120, 30)
	m.Draw(scr, scr.Bounds())
	return m
}

// zoneFor returns the hit zone of a toolbar button, so a test can click
// exactly where the last frame drew it.
func zoneFor(t *testing.T, m *Dashboard, action threadAction) threadsHitZone {
	t.Helper()
	for _, z := range m.zones {
		if !z.isFilter && z.action == action {
			return z
		}
	}
	t.Fatalf("no hit zone for action %v", action)
	return threadsHitZone{}
}

func clickAt(m *Dashboard, pt image.Point) (bool, tea.Cmd) {
	return m.HandleMouseClick(tea.MouseClickMsg{X: pt.X, Y: pt.Y, Button: tea.MouseLeft})
}

func TestThreadsDashboardClickRunsAction(t *testing.T) {
	t.Parallel()

	m := dashboardWith(t, proto.Thread{ID: "t1", Name: "one", Kind: "thread", Status: "running", SessionID: "sess"})

	z := zoneFor(t, m, actionOpen)
	handled, cmd := clickAt(m, z.rect.Min)
	require.True(t, handled)
	require.NotNil(t, cmd)
	msg, ok := cmd().(EnterMsg)
	require.True(t, ok)
	require.Equal(t, "t1", msg.ID)
	require.Equal(t, "sess", msg.SessionID)
}

func TestThreadsDashboardClickRowSelectsOnly(t *testing.T) {
	t.Parallel()

	m := dashboardWith(t,
		proto.Thread{ID: "t1", Name: "one", Kind: "thread", Status: "running"},
		proto.Thread{ID: "t2", Name: "two", Kind: "thread", Status: "running"},
	)
	require.Equal(t, "t1", m.selected().ID)

	second := image.Pt(m.listRect.Min.X+2, m.listRect.Min.Y+1)
	handled, cmd := clickAt(m, second)
	require.True(t, handled)
	require.Nil(t, cmd)
	require.Equal(t, "t2", m.selected().ID)
}

// TestThreadsDashboardSelectionSurvivesRefresh proves a status event
// landing mid-triage does not throw the operator's selection back to the
// top of the list.
func TestThreadsDashboardSelectionSurvivesRefresh(t *testing.T) {
	t.Parallel()

	m := dashboardWith(t,
		proto.Thread{ID: "t1", Name: "one", Kind: "thread", Status: "running"},
		proto.Thread{ID: "t2", Name: "two", Kind: "thread", Status: "running"},
	)
	m.list.SetSelected(1)
	require.Equal(t, "t2", m.selected().ID)

	m.cache.Cache.Value = []proto.Thread{
		{ID: "t1", Name: "one", Kind: "thread", Status: "running"},
		{ID: "t2", Name: "two", Kind: "thread", Status: "completed"},
	}
	m.RebuildItems()
	require.Equal(t, "t2", m.selected().ID)
}

// TestSelectedSurvivesConcurrentCacheDelete covers a regression: selected()
// used to return &m.visible[idx], which under the All filter aliases the
// ListCache's backing array directly (see filterThreads). ApplyEvent's
// DeletedEvent handler removes from that array in place with
// append(value[:i], value[i+1:]...), which shifts every element after the
// deleted one down by one slot — silently overwriting whatever a
// previously-taken pointer into a later index pointed at, without ever
// assigning through that pointer. selected() must hand back a copy instead.
func TestSelectedSurvivesConcurrentCacheDelete(t *testing.T) {
	t.Parallel()

	m := dashboardWith(t,
		proto.Thread{ID: "t1", Name: "one", Kind: "thread", Status: "running"},
		proto.Thread{ID: "t2", Name: "two", Kind: "thread", Status: "running"},
		proto.Thread{ID: "t3", Name: "three", Kind: "thread", Status: "running"},
	)
	m.list.SetSelected(1)
	sel := m.selected()
	require.Equal(t, "t2", sel.ID)

	// Deleting the entry *before* the selected one shifts every later
	// element (t3) down by one slot in the cache's backing array. If sel
	// aliases that array, its target is now t3's data even though nothing
	// ever assigned through sel.
	m.cache.ApplyEvent(pubsub.Event[proto.Thread]{
		Type:    pubsub.DeletedEvent,
		Payload: proto.Thread{ID: "t1", Kind: "thread"},
	})

	require.Equal(t, "t2", sel.ID,
		"a pointer returned by selected() must not change out from under the caller")
}

// TestComputeThreadsColumnsDropsColumnsWhenNarrow proves the table sheds
// columns from the right as the terminal narrows, so name and status —
// the two fields the screen is scanned by — always survive.
func TestComputeThreadsColumnsDropsColumnsWhenNarrow(t *testing.T) {
	t.Parallel()

	wide := computeThreadsColumns(140)
	require.Positive(t, wide.branch)
	require.Positive(t, wide.updated)
	require.Positive(t, wide.goal)

	medium := computeThreadsColumns(70)
	require.Zero(t, medium.branch, "branch is the first column to go")
	require.Positive(t, medium.name)

	narrow := computeThreadsColumns(30)
	require.Zero(t, narrow.branch)
	require.Zero(t, narrow.updated)
	require.Positive(t, narrow.name)
	require.Positive(t, narrow.status)
}

// TestThreadsDashboardBackButtonLeaves proves the Back button raises the
// same transition esc does, as a message for the router to act on.
func TestThreadsDashboardBackButtonLeaves(t *testing.T) {
	t.Parallel()

	m := dashboardWith(t, proto.Thread{ID: "t1", Name: "one", Kind: "thread", Status: "running"})
	z := zoneFor(t, m, actionBack)
	handled, cmd := clickAt(m, z.rect.Min)
	require.True(t, handled)
	require.NotNil(t, cmd)
	_, ok := cmd().(LeaveMsg)
	require.True(t, ok)
}

// TestThreadsDashboardEmptyStateNamesTheReason proves the empty table
// distinguishes "nothing exists" from "a filter is hiding it" — the two
// need different next steps.
func TestThreadsDashboardEmptyStateNamesTheReason(t *testing.T) {
	t.Parallel()

	m := dashboardWith(t)
	empty := m.emptyText()
	require.Equal(t, "No delegations yet.", empty)
	require.NotContains(t, empty, "press n")
	require.NotContains(t, empty, "+ New")
	require.NotContains(t, strings.ToLower(empty), "create")

	m = dashboardWith(t, proto.Thread{ID: "t1", Name: "one", Kind: "thread", Status: "running"})
	m.setFilter(filterFailed)
	filtered := m.emptyText()
	require.Equal(t, "No failed delegations — press a to show all.", filtered)
	require.NotContains(t, filtered, "press n")
	require.NotContains(t, filtered, "+ New")
}
