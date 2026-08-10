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
	m.cache.threads = []proto.Thread{{ID: "s1", Name: "one"}}
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
	m.cache.threads = []proto.Thread{{ID: "s1", Status: "completed"}}
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
	m.cache.threads = []proto.Thread{{ID: "s1", Status: "merging"}}
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
	m.cache.threads = []proto.Thread{{ID: "s1"}}
	m.rebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "x", Code: 'x'})
	require.True(t, handled)
	require.NotNil(t, cmd)
	msg, ok := cmd().(removeThreadMsg)
	require.True(t, ok)
	require.Equal(t, "s1", msg.id)
}

func TestThreadsDashboardHandleKeyReload(t *testing.T) {
	t.Parallel()

	ws := &threadsTestWorkspace{supported: true}
	m := newTestThreadsDashboard(t, ws)
	m.cache.checkedAt = time.Now() // fresh cache would normally skip a refresh

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
	cmds := m.ApplyThreadsLoaded(threadsLoadedMsg{gen: m.cache.gen, threads: threads})
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
