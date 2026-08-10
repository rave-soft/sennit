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

func TestStrandItemRenderRespectsWidth(t *testing.T) {
	t.Parallel()

	item := newStrandItem(testStyles(), proto.Strand{
		ID:     "s1",
		Name:   "add-auth",
		Status: "running",
		Branch: "strand/add-auth",
		Goal:   "Implement OAuth login end to end across every service",
	})

	for _, width := range []int{40, 120} {
		require.NotPanics(t, func() {
			rendered := item.Render(width)
			require.LessOrEqual(t, ansi.StringWidth(rendered), width)
		})
	}
}

func TestStrandItemRenderTruncatesGoal(t *testing.T) {
	t.Parallel()

	longGoal := strings.Repeat("implement a very long goal description ", 10)
	item := newStrandItem(testStyles(), proto.Strand{
		ID:     "s1",
		Name:   "add-auth",
		Status: "running",
		Branch: "strand/add-auth",
		Goal:   longGoal,
	})

	rendered := item.Render(60)
	require.LessOrEqual(t, ansi.StringWidth(rendered), 60)
	require.Contains(t, ansi.Strip(rendered), "…", "goal should be truncated with an ellipsis marker")
}

func newTestStrandsDashboard(t *testing.T, ws *strandsTestWorkspace) *strandsDashboard {
	t.Helper()
	com := &common.Common{Workspace: ws, Styles: testStyles()}
	m := newStrandsDashboard(com)
	m.SetSize(80, 20)
	return m
}

func TestStrandsDashboardHandleKeyEnter(t *testing.T) {
	t.Parallel()

	ws := &strandsTestWorkspace{supported: true}
	m := newTestStrandsDashboard(t, ws)
	m.cache.strands = []proto.Strand{{ID: "s1", Name: "one"}}
	m.rebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.True(t, handled)
	require.NotNil(t, cmd)
	msg, ok := cmd().(enterStrandMsg)
	require.True(t, ok)
	require.Equal(t, "s1", msg.id)
}

func TestStrandsDashboardHandleKeyNew(t *testing.T) {
	t.Parallel()

	ws := &strandsTestWorkspace{supported: true}
	m := newTestStrandsDashboard(t, ws)

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "n", Code: 'n'})
	require.True(t, handled)
	require.NotNil(t, cmd)
	_, ok := cmd().(openStrandCreateMsg)
	require.True(t, ok)
}

func TestStrandsDashboardHandleKeyMerge(t *testing.T) {
	t.Parallel()

	ws := &strandsTestWorkspace{supported: true}
	m := newTestStrandsDashboard(t, ws)
	m.cache.strands = []proto.Strand{{ID: "s1", Status: "completed"}}
	m.rebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "m", Code: 'm'})
	require.True(t, handled)
	require.NotNil(t, cmd)
	msg, ok := cmd().(mergeStrandMsg)
	require.True(t, ok)
	require.Equal(t, "s1", msg.id)
}

func TestStrandsDashboardHandleKeyMergeSkipsAlreadyMerging(t *testing.T) {
	t.Parallel()

	ws := &strandsTestWorkspace{supported: true}
	m := newTestStrandsDashboard(t, ws)
	m.cache.strands = []proto.Strand{{ID: "s1", Status: "merging"}}
	m.rebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "m", Code: 'm'})
	require.True(t, handled)
	require.Nil(t, cmd, "already-merging strand should not re-trigger a merge")
}

func TestStrandsDashboardHandleKeyRemove(t *testing.T) {
	t.Parallel()

	ws := &strandsTestWorkspace{supported: true}
	m := newTestStrandsDashboard(t, ws)
	m.cache.strands = []proto.Strand{{ID: "s1"}}
	m.rebuildItems()
	m.list.SelectFirst()

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "x", Code: 'x'})
	require.True(t, handled)
	require.NotNil(t, cmd)
	msg, ok := cmd().(removeStrandMsg)
	require.True(t, ok)
	require.Equal(t, "s1", msg.id)
}

func TestStrandsDashboardHandleKeyReload(t *testing.T) {
	t.Parallel()

	ws := &strandsTestWorkspace{supported: true}
	m := newTestStrandsDashboard(t, ws)
	m.cache.checkedAt = time.Now() // fresh cache would normally skip a refresh

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "r", Code: 'r'})
	require.True(t, handled)
	require.NotNil(t, cmd, "'r' should force a refresh even when the cache is fresh")
}

func TestStrandsDashboardHandleKeyUnrecognized(t *testing.T) {
	t.Parallel()

	ws := &strandsTestWorkspace{supported: true}
	m := newTestStrandsDashboard(t, ws)

	handled, cmd := m.HandleKey(tea.KeyPressMsg{Text: "z", Code: 'z'})
	require.False(t, handled)
	require.Nil(t, cmd)
}

func TestStrandsDashboardSetActiveDispatchesRefreshWhenStale(t *testing.T) {
	t.Parallel()

	ws := &strandsTestWorkspace{supported: true}
	m := newTestStrandsDashboard(t, ws)

	cmd := m.SetActive(true)
	require.NotNil(t, cmd, "an empty/stale cache should trigger a refresh on activation")
	require.True(t, m.active)
}

func TestStrandsDashboardApplyStrandsLoadedRebuildsItems(t *testing.T) {
	t.Parallel()

	ws := &strandsTestWorkspace{supported: true}
	m := newTestStrandsDashboard(t, ws)

	strands := []proto.Strand{{ID: "s1", Name: "one"}, {ID: "s2", Name: "two"}}
	cmds := m.ApplyStrandsLoaded(strandsLoadedMsg{gen: m.cache.gen, strands: strands})
	require.Nil(t, cmds)
	require.Equal(t, 2, m.list.Len())
}

func TestStrandsDashboardApplyStrandEventRebuildsItems(t *testing.T) {
	t.Parallel()

	ws := &strandsTestWorkspace{supported: true}
	m := newTestStrandsDashboard(t, ws)
	m.active = true

	cmd := m.ApplyStrandEvent(pubsub.Event[proto.Strand]{
		Type:    pubsub.CreatedEvent,
		Payload: proto.Strand{ID: "s1", Name: "one"},
	})
	require.Equal(t, 1, m.list.Len())
	require.NotNil(t, cmd, "active dashboard should re-arm a refresh after the event invalidates the TTL")
}
