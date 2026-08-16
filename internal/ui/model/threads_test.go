package model

import (
	"image"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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

// TestThreadMergeableTaskAlwaysFalse proves a task never reports mergeable
// regardless of status: it has no worktree/branch of its own to merge (see
// TaskController's doc comment).
func TestThreadMergeableTaskAlwaysFalse(t *testing.T) {
	t.Parallel()

	for _, status := range []string{"pending", "running", "idle", "completed", "failed"} {
		require.Falsef(t, threadMergeable("task", status), "status=%s", status)
	}
}

// TestThreadMergeableThreadUnaffected proves the kind change didn't alter a
// thread's existing merge-eligibility rules.
func TestThreadMergeableThreadUnaffected(t *testing.T) {
	t.Parallel()

	require.True(t, threadMergeable("thread", "completed"))
	require.False(t, threadMergeable("thread", "merged"))
	require.False(t, threadMergeable("thread", "merging"))
}

// TestThreadStatusStyleIdleIsNeitherDoneNorError proves idle takes its own
// neutral color rather than reading as a finished or broken delegation:
// it is a live thread with no run in flight.
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
func TestThreadStatusStyleClasses(t *testing.T) {
	t.Parallel()

	sty := testStyles()
	for status, want := range map[string]lipgloss.Style{
		"running":       sty.Threads.StatusRunning,
		"merging":       sty.Threads.StatusRunning,
		"completed":     sty.Threads.StatusDone,
		"merged":        sty.Threads.StatusDone,
		"failed":        sty.Threads.StatusError,
		"conflict":      sty.Threads.StatusError,
		"merge_blocked": sty.Threads.StatusError,
		"cancelled":     sty.Threads.StatusWarn,
		"interrupted":   sty.Threads.StatusWarn,
	} {
		require.Equalf(t, want, threadStatusStyle(sty, status), "status=%s", status)
	}
}

func newTestThreadsDashboard(t *testing.T, ws *threadsTestWorkspace) *threadsDashboard {
	t.Helper()
	com := &common.Common{Workspace: ws, Styles: testStyles()}
	m := newThreadsDashboard(com)
	m.SetSize(80, 20)
	return m
}

func TestThreadsDashboardHandleKeyEnter(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.cache.cache.value = []proto.Thread{{ID: "s1", Name: "one"}}
	m.rebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.True(t, handled)
	require.NotNil(t, cmd)
	msg, ok := cmd().(enterThreadMsg)
	require.True(t, ok)
	require.Equal(t, "s1", msg.id)
}

func TestThreadsDashboardHandleKeyNew(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "n", Code: 'n'})
	require.True(t, handled)
	require.NotNil(t, cmd)
	_, ok := cmd().(openThreadCreateMsg)
	require.True(t, ok)
}

func TestThreadsDashboardHandleKeyMerge(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.cache.cache.value = []proto.Thread{{ID: "s1", Status: "completed"}}
	m.rebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "m", Code: 'm'})
	require.True(t, handled)
	require.NotNil(t, cmd)
	msg, ok := cmd().(mergeThreadMsg)
	require.True(t, ok)
	require.Equal(t, "s1", msg.id)
}

func TestThreadsDashboardHandleKeyMergeSkipsAlreadyMerging(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.cache.cache.value = []proto.Thread{{ID: "s1", Status: "merging"}}
	m.rebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "m", Code: 'm'})
	require.True(t, handled)
	require.Nil(t, cmd, "already-merging thread should not re-trigger a merge")
}

func TestThreadsDashboardHandleKeyRemove(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.cache.cache.value = []proto.Thread{{ID: "s1"}}
	m.rebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "x", Code: 'x'})
	require.True(t, handled)
	require.NotNil(t, cmd)
	msg, ok := cmd().(confirmRemoveThreadMsg)
	require.True(t, ok, "x should request confirmation, not remove directly")
	require.Equal(t, "s1", msg.id)
}

// TestThreadsDashboardHandleKeyCancelTask proves the cancel key emits
// cancelDelegationMsg for a non-terminal task row.
func TestThreadsDashboardHandleKeyCancelTask(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.cache.cache.value = []proto.Thread{{ID: "t1", Kind: "task", Status: "running"}}
	m.rebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "c", Code: 'c'})
	require.True(t, handled)
	require.NotNil(t, cmd)
	msg, ok := cmd().(cancelDelegationMsg)
	require.True(t, ok)
	require.Equal(t, "t1", msg.id)
	require.Equal(t, "task", msg.kind)
}

// TestThreadsDashboardHandleKeyCancelThread proves the cancel key also
// emits cancelDelegationMsg for a non-terminal thread row: unlike a task,
// cancelling a thread leaves its worktree and branch on disk rather than
// tearing it down (see Manager.Cancel), so there is no reason to withhold
// the key from a thread row the way an earlier step did.
func TestThreadsDashboardHandleKeyCancelThread(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.cache.cache.value = []proto.Thread{{ID: "s1", Kind: "thread", Status: "running"}}
	m.rebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "c", Code: 'c'})
	require.True(t, handled)
	require.NotNil(t, cmd)
	msg, ok := cmd().(cancelDelegationMsg)
	require.True(t, ok)
	require.Equal(t, "s1", msg.id)
	require.Equal(t, "thread", msg.kind)
}

// TestThreadsDashboardHandleKeyCancelSkipsTerminalTask proves cancel is a
// no-op for a task that's already reached a terminal status.
func TestThreadsDashboardHandleKeyCancelSkipsTerminalTask(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.cache.cache.value = []proto.Thread{{ID: "t1", Kind: "task", Status: "completed"}}
	m.rebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "c", Code: 'c'})
	require.True(t, handled)
	require.Nil(t, cmd, "an already-terminal task should not re-trigger a cancel")
}

// TestThreadsDashboardHandleKeyCancelSkipsTerminalThread is
// TestThreadsDashboardHandleKeyCancelSkipsTerminalTask's thread-kind
// sibling.
func TestThreadsDashboardHandleKeyCancelSkipsTerminalThread(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.cache.cache.value = []proto.Thread{{ID: "s1", Kind: "thread", Status: "merged"}}
	m.rebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "c", Code: 'c'})
	require.True(t, handled)
	require.Nil(t, cmd, "an already-terminal thread should not re-trigger a cancel")
}

// TestCancelDelegationCmdCallsWorkspaceOnce drives the router end to end
// for a task: a cancelDelegationMsg for one task's id must call
// Workspace.CancelTask exactly once with that id — never any other
// delegation's id, never CancelThread, and nothing about this path
// touches Escape/the foreground-turn cancel.
func TestCancelDelegationCmdCallsWorkspaceOnce(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	com := &common.Common{Workspace: ws, Styles: testStyles()}
	r := &Root{com: com, dashboardDialog: nil}

	_, cmd := r.Update(cancelDelegationMsg{id: "t1", kind: "task"})
	require.NotNil(t, cmd)

	msg := cmd()
	done, ok := msg.(threadActionDoneMsg)
	require.True(t, ok)
	require.NoError(t, done.err)

	require.Equal(t, []string{"t1"}, ws.cancelTaskCalls, "CancelTask must be called exactly once, with the selected task's id")
	require.Empty(t, ws.cancelThreadCalls, "a task cancel must never call CancelThread")
}

// TestCancelDelegationCmdRoutesThreadKindToCancelThread is
// TestCancelDelegationCmdCallsWorkspaceOnce's thread-kind sibling: a
// cancelDelegationMsg for a thread must call Workspace.CancelThread, not
// CancelTask.
func TestCancelDelegationCmdRoutesThreadKindToCancelThread(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	com := &common.Common{Workspace: ws, Styles: testStyles()}
	r := &Root{com: com, dashboardDialog: nil}

	_, cmd := r.Update(cancelDelegationMsg{id: "s1", kind: "thread"})
	require.NotNil(t, cmd)

	msg := cmd()
	done, ok := msg.(threadActionDoneMsg)
	require.True(t, ok)
	require.NoError(t, done.err)

	require.Equal(t, []string{"s1"}, ws.cancelThreadCalls, "CancelThread must be called exactly once, with the selected thread's id")
	require.Empty(t, ws.cancelTaskCalls, "a thread cancel must never call CancelTask")
}

func TestThreadsDashboardHandleKeyReload(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.cache.cache.timestamp = time.Now() // fresh cache would normally skip a refresh

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
	cmds := m.ApplyThreadsLoaded(threadsLoadedMsg{gen: m.cache.cache.generation, threads: threads})
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
func dashboardWith(t *testing.T, threads ...proto.Thread) *threadsDashboard {
	t.Helper()
	m := newTestThreadsDashboard(t, &threadsTestWorkspace{supported: true})
	m.SetSize(120, 30)
	m.cache.cache.value = threads
	m.rebuildItems()
	scr := uv.NewScreenBuffer(120, 30)
	m.Draw(scr, scr.Bounds())
	return m
}

// zoneFor returns the hit zone of a toolbar button, so a test can click
// exactly where the last frame drew it.
func zoneFor(t *testing.T, m *threadsDashboard, action threadAction) threadsHitZone {
	t.Helper()
	for _, z := range m.zones {
		if !z.isFilter && z.action == action {
			return z
		}
	}
	t.Fatalf("no hit zone for action %v", action)
	return threadsHitZone{}
}

// zoneForFilter returns the hit zone of a filter tab.
func zoneForFilter(t *testing.T, m *threadsDashboard, f threadsFilter) threadsHitZone {
	t.Helper()
	for _, z := range m.zones {
		if z.isFilter && z.filter == f {
			return z
		}
	}
	t.Fatalf("no hit zone for filter %v", f)
	return threadsHitZone{}
}

func clickAt(m *threadsDashboard, pt image.Point) (bool, tea.Cmd) {
	return m.HandleMouseClick(tea.MouseClickMsg{X: pt.X, Y: pt.Y, Button: tea.MouseLeft})
}

// TestThreadsToolbarEnablementFollowsSelection is the contract the toolbar
// and the key bindings share: an action is offered exactly when it can
// actually run. A dimmed button and a working shortcut (or the reverse)
// would be the bug.
func TestThreadsToolbarEnablementFollowsSelection(t *testing.T) {
	t.Parallel()

	running := proto.Thread{ID: "t1", Kind: "thread", Status: "running"}
	merged := proto.Thread{ID: "t2", Kind: "thread", Status: "merged"}
	task := proto.Thread{ID: "t3", Kind: "task", Status: "running"}

	require.True(t, actionNew.enabledFor(nil), "new never needs a selection")
	require.True(t, actionRefresh.enabledFor(nil))
	require.True(t, actionBack.enabledFor(nil))
	require.False(t, actionOpen.enabledFor(nil), "nothing selected, nothing to open")
	require.False(t, actionMerge.enabledFor(nil))
	require.False(t, actionCancel.enabledFor(nil))
	require.False(t, actionRemove.enabledFor(nil))

	require.True(t, actionMerge.enabledFor(&running))
	require.False(t, actionMerge.enabledFor(&merged), "already merged")
	require.False(t, actionMerge.enabledFor(&task), "a task has no branch to merge")

	require.True(t, actionCancel.enabledFor(&running))
	require.False(t, actionCancel.enabledFor(&merged), "terminal, nothing in flight")
}

// TestThreadsDashboardClickRunsAction proves a toolbar click produces the
// same message the keyboard shortcut does.
func TestThreadsDashboardClickRunsAction(t *testing.T) {
	t.Parallel()

	m := dashboardWith(t, proto.Thread{ID: "t1", Name: "one", Kind: "thread", Status: "running", SessionID: "sess"})

	z := zoneFor(t, m, actionOpen)
	handled, cmd := clickAt(m, z.rect.Min)
	require.True(t, handled)
	require.NotNil(t, cmd)
	msg, ok := cmd().(enterThreadMsg)
	require.True(t, ok)
	require.Equal(t, "t1", msg.id)
	require.Equal(t, "sess", msg.sessionID)
}

// TestThreadsDashboardClickDisabledButtonDoesNothing proves a dimmed
// button is inert and — crucially — swallows the click rather than letting
// it fall through to the row underneath.
func TestThreadsDashboardClickDisabledButtonDoesNothing(t *testing.T) {
	t.Parallel()

	m := dashboardWith(t, proto.Thread{ID: "t1", Name: "one", Kind: "task", Status: "running"})

	z := zoneFor(t, m, actionMerge)
	require.False(t, z.enabled, "a task is never mergeable")
	handled, cmd := clickAt(m, z.rect.Min)
	require.True(t, handled)
	require.Nil(t, cmd)
}

// TestThreadsDashboardClickTabFilters proves the tabs narrow the table and
// that the counts they carry match what filtering actually yields.
func TestThreadsDashboardClickTabFilters(t *testing.T) {
	t.Parallel()

	m := dashboardWith(t,
		proto.Thread{ID: "t1", Name: "one", Kind: "thread", Status: "running"},
		proto.Thread{ID: "t2", Name: "two", Kind: "thread", Status: "merged"},
		proto.Thread{ID: "t3", Name: "three", Kind: "thread", Status: "failed"},
	)
	require.Equal(t, 3, m.list.Len())

	z := zoneForFilter(t, m, filterFailed)
	handled, cmd := clickAt(m, z.rect.Min)
	require.True(t, handled)
	require.Nil(t, cmd)
	require.Equal(t, filterFailed, m.filter)
	require.Equal(t, 1, m.list.Len())
	require.Equal(t, "t3", m.selected().ID)
}

// TestThreadsDashboardClickRowSelectsOnly proves clicking a row moves the
// selection and nothing else: acting is what the buttons are for, so a
// stray click can never merge or remove a thread.
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

	m.cache.cache.value = []proto.Thread{
		{ID: "t1", Name: "one", Kind: "thread", Status: "running"},
		{ID: "t2", Name: "two", Kind: "thread", Status: "completed"},
	}
	m.rebuildItems()
	require.Equal(t, "t2", m.selected().ID)
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
	_, ok := cmd().(leaveDashboardMsg)
	require.True(t, ok)
}

// TestThreadsDashboardEmptyStateNamesTheReason proves the empty table
// distinguishes "nothing exists" from "a filter is hiding it" — the two
// need different next steps.
func TestThreadsDashboardEmptyStateNamesTheReason(t *testing.T) {
	t.Parallel()

	m := dashboardWith(t)
	require.Contains(t, m.emptyText(), "No threads yet")

	m = dashboardWith(t, proto.Thread{ID: "t1", Name: "one", Kind: "thread", Status: "running"})
	m.setFilter(filterFailed)
	require.Contains(t, m.emptyText(), "failed")
}
