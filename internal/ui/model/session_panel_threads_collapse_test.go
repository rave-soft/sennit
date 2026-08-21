package model

import (
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// TestSessionPanelPlan_CollapsedThreadsKeepHeaderDropBlocks covers the
// threads section's collapse, which mirrors the todos section's: the
// blocks go, the header stays — it is both the count and the only thing
// left to click to bring them back — and the reclaimed rows are genuinely
// released rather than reserved.
func TestSessionPanelPlan_CollapsedThreadsKeepHeaderDropBlocks(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.threadList.cache.value = mkDockThreads(2)

	expanded := u.sessionPanelPlan(100)
	require.True(t, expanded.threadsExpanded)
	require.Equal(t, 4, expanded.threadsRows, "two blocks, two rows each")
	require.Equal(t, 1, expanded.threadsHeaderRows)
	require.Equal(t, 2, expanded.threadsActive)

	u.toggleThreadsCollapsed()
	collapsed := u.sessionPanelPlan(100)
	require.False(t, collapsed.threadsExpanded)
	require.Zero(t, collapsed.threadsRows, "collapsed blocks must not reserve rows")
	require.Equal(t, 1, collapsed.threadsHeaderRows, "the header survives so it can be clicked again")
	require.Equal(t, 2, collapsed.threadsActive, "the header still reports what is running")
	require.Less(t, collapsed.totalRows, expanded.totalRows, "collapsing must actually reclaim space")

	u.toggleThreadsCollapsed()
	require.Equal(t, expanded.totalRows, u.sessionPanelPlan(100).totalRows, "toggling back restores the section")
}

// TestSessionPanelPlan_CollapsedThreadsWithNoneActiveDropHeader pins the
// difference between "the user collapsed this" and "there is nothing
// here": with no active threads the header goes too, rather than leaving
// a row saying zero.
func TestSessionPanelPlan_CollapsedThreadsWithNoneActiveDropHeader(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.toggleThreadsCollapsed()
	plan := u.sessionPanelPlan(100)
	require.Zero(t, plan.threadsHeaderRows)
	require.Zero(t, plan.threadsRows)
}

// TestDrawSessionPanel_CollapsedThreadsHeaderIsClickable proves the
// collapsed header is painted where the layout says it is, so the click
// that expands it again lands on the row the user sees.
func TestDrawSessionPanel_CollapsedThreadsHeaderIsClickable(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.threadList.cache.value = mkDockThreads(2)
	u.toggleThreadsCollapsed()

	area := uv.Rect(0, 0, 60, 6)
	u.lay.layout.panel = area
	plan := u.sessionPanelPlan(area.Dy())
	_, _, _, threadsHeaderRect := sessionPanelRowLayout(area, plan)
	require.False(t, threadsHeaderRect.Empty(), "a collapsed section still needs a hit target")

	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	u.drawSessionPanel(scr, area)
	painted := ansi.Strip(scr.Render())
	require.Contains(t, painted, "threads 2", "the header reports the active count")
	require.Contains(t, painted, "▸", "and shows it is collapsed")
	require.Equal(t, area.Min.Y, threadsHeaderRect.Min.Y, "the hit target is the row it paints on")
}
