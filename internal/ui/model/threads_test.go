package model

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func testStyles() *styles.Styles {
	s := styles.ThemeForProvider("")
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
	})

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
	})

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

// TestThreadBadgeIdleIsExplicitAndNotDone proves the dashboard's idle badge
// takes its own explicit path rather than reading as a finished
// (SuccessMessage-styled) delegation.
func TestThreadBadgeIdleIsExplicitAndNotDone(t *testing.T) {
	t.Parallel()

	sty := testStyles()
	idle := threadBadge(sty, "idle")
	require.Contains(t, idle, "IDLE")
	require.NotEqual(t, threadBadge(sty, "completed"), idle)
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
