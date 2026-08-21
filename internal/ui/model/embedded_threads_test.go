package model

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/stretchr/testify/require"
)

// TestEmbeddedThreadUI_ShowsNoThreadsInItsPanel: threads belong to the
// workspace above a thread, not inside one. Listing a thread's siblings
// while you are looking at its work says nothing about that work, and
// offering to open them from there invites threads within threads.
func TestEmbeddedThreadUI_ShowsNoThreadsInItsPanel(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.threadList.cache.value = mkDockThreads(2)
	require.Positive(t, u.sessionPanelPlan(100).threadsActive,
		"precondition: the main screen does show them")

	u.embedded = true
	plan := u.sessionPanelPlan(100)
	require.Zero(t, plan.threadsActive)
	require.Empty(t, plan.threads)
	require.Zero(t, plan.threadsRows)
	require.Zero(t, plan.threadsHeaderRows, "not even the header, which is only there to expand them")
}

// TestEmbeddedThreadUI_ShowsNoThreadBadge: same reasoning as the panel —
// the count in the header is about the workspace above this one.
func TestEmbeddedThreadUI_ShowsNoThreadBadge(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.threadList.cache.set([]proto.Thread{{ID: "s1", Status: "running"}, {ID: "s2", Status: "pending"}, {ID: "s3", Status: "merging"}})
	require.Equal(t, 3, u.activeThreadBadgeCount())

	u.embedded = true
	require.Zero(t, u.activeThreadBadgeCount())
}

// TestEmbeddedThreadUI_StartsNoThreadRefreshes: it renders none of it, so
// it pays for none of it. Attaching into each listed thread to read its
// live activity is the expensive half, and doing it from inside a thread
// buys nothing.
func TestEmbeddedThreadUI_StartsNoThreadRefreshes(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.com.Workspace = &threadsTestWorkspace{supported: true}
	u.state = uiChat
	require.NotEmpty(t, u.threadViewsRefreshCmds(), "precondition: the main screen does refresh them")

	u.embedded = true
	require.Empty(t, u.threadViewsRefreshCmds())
}

// TestRoot_MainScreenResultsArriveWhileAThreadIsOpen is the regression
// test for a panel that goes stale and never recovers.
//
// The main screen's threads panel refreshes off-thread. Routing the
// result by whichever screen is on top hands it to the thread's embedded
// UI instead — and the fetch that produced it stays marked in flight
// forever, because only its own result would have cleared that mark. From
// then on the panel behind you never updates again: threads the agent
// created do not appear, ones it removed do not leave.
func TestRoot_MainScreenResultsArriveWhileAThreadIsOpen(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	threadUI := New(common.DefaultCommon(context.Background(), &rootTestWorkspace{}), "", false, WithEmbedded())
	r.thread = &threadAttachment{threadID: "t1", ui: threadUI}
	r.active = screenThread

	// A refresh the main screen started before the user drilled in.
	gen, started := r.main.threadList.cache.begin()
	require.True(t, started)

	r.Update(threadsLoadedMsg{gen: gen, threads: mkDockThreads(2)})

	require.False(t, r.main.threadList.cache.inFlight,
		"the result must reach the screen that asked, or its next refresh never starts")
	require.Len(t, r.main.threadList.cache.value, 2)
	require.Empty(t, threadUI.threadList.cache.value,
		"and it must not land in the thread's own state")
}

// TestRoot_MainScreenResultsSurviveTheDashboardToo: the dashboard screen
// must not drop the shared cache's result on the floor either — it is
// explicitly routed to r.main regardless of which screen is on top (see
// root.go's threadsLoadedMsg case), the same guarantee
// TestRoot_MainScreenResultsArriveWhileAThreadIsOpen pins for screenThread.
func TestRoot_MainScreenResultsSurviveTheDashboardToo(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	r.active = screenDashboard

	gen, started := r.main.threadList.cache.begin()
	require.True(t, started)

	r.Update(threadsLoadedMsg{gen: gen, threads: mkDockThreads(4)})

	require.False(t, r.main.threadList.cache.inFlight)
	require.Len(t, r.main.threadList.cache.value, 4)
}

// TestRoot_ScreenBoundMessagesStillFollowTheActiveScreen: only results
// claimed by the main screen are re-routed. Everything else — input, and
// whatever the visible screen is doing — still goes where it was going.
func TestRoot_ScreenBoundMessagesStillFollowTheActiveScreen(t *testing.T) {
	t.Parallel()

	r := newTestRoot(t, true)
	threadUI := New(common.DefaultCommon(context.Background(), &rootTestWorkspace{}), "", false, WithEmbedded())
	r.thread = &threadAttachment{threadID: "t1", ui: threadUI}
	r.active = screenThread

	r.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	require.Equal(t, 100, threadUI.lay.width, "the visible screen still gets its own messages")
}
