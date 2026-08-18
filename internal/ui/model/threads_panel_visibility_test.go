package model

import (
	"testing"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// TestSessionPanelPlan_ShedThreadsStayWholeBlocksAndAreCounted covers what
// a cramped panel does with more threads than fit. Every block is two rows
// (name, then status), so shedding by single rows left a name with no
// status under it, and the threads that fell off were not reported
// anywhere: the "…and N more" footer only ever counted the ones the
// visible cap dropped.
func TestSessionPanelPlan_ShedThreadsStayWholeBlocksAndAreCounted(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.threadsDock.cache.value = mkDockThreads(4)

	full := u.sessionPanelPlan(100)
	require.Len(t, full.threads, 4)
	require.Equal(t, 8, full.threadsRows)
	require.Zero(t, full.threadsMore)

	// A budget that cannot hold all four: the survivors must still be
	// whole blocks, and every dropped one has to show up in the count.
	tight := u.sessionPanelPlan(6)
	require.Zero(t, tight.threadsRows%2-1, "a footer row makes the total odd; blocks themselves stay paired")
	require.Equal(t, len(tight.threads)*2+1, tight.threadsRows)
	require.Equal(t, 4-len(tight.threads), tight.threadsMore,
		"threads dropped by the budget must be counted, not silently hidden")
	require.LessOrEqual(t, tight.totalRows, 6)
}

// TestThreadsDock_RemovedThreadLeavesThePanelImmediately covers the row
// that outlives its thread. Removal used to only mark the cached list
// stale, so until a re-list landed the panel kept offering an entry that
// could not be opened — attaching resolves the id and finds nothing.
func TestThreadsDock_RemovedThreadLeavesThePanelImmediately(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.threadsDock.cache.value = mkDockThreads(3)
	require.Len(t, u.sessionPanelPlan(100).threads, 3)

	gone := u.threadsDock.cache.value[1]
	u.threadsDock.applyThreadEvent(pubsub.Event[proto.Thread]{
		Type:    pubsub.DeletedEvent,
		Payload: gone,
	})

	plan := u.sessionPanelPlan(100)
	require.Len(t, plan.threads, 2, "the removed thread must be gone from this frame, not the next refresh")
	for _, shown := range plan.threads {
		require.NotEqual(t, gone.ID, shown.ID)
	}
}

// TestThreadsDock_StatusEventStillOnlyInvalidates pins the other half of
// that rule: a status change is ordinary staleness and must keep waiting
// for the authoritative re-list rather than being applied optimistically.
func TestThreadsDock_StatusEventStillOnlyInvalidates(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.threadsDock.cache.value = mkDockThreads(2)
	changed := u.threadsDock.cache.value[0]
	changed.Status = "merged"

	u.threadsDock.applyThreadEvent(pubsub.Event[proto.Thread]{
		Type:    pubsub.UpdatedEvent,
		Payload: changed,
	})
	require.Len(t, u.threadsDock.cache.value, 2, "an update must not drop the row")
}

// Drilling into a sub-agent's transcript must not bring the threads block
// with it. That view is somebody else's turn, read-only, and the block is
// a way into the threads of the session you are driving -- there is no
// driving there.
func TestSessionPanelPlan_NoThreadsBlockInASubAgentTranscript(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.threadsDock.cache.value = mkDockThreads(3)
	require.Len(t, u.sessionPanelPlan(100).threads, 3, "precondition: the block shows on the session itself")

	u.sess.navStack = append(u.sess.navStack, sessionNavFrame{})

	plan := u.sessionPanelPlan(100)
	require.Empty(t, plan.threads)
	require.Zero(t, plan.threadsRows)
	require.Zero(t, plan.threadsActive)
}

// Coming back out restores it: the block belongs to the session being
// driven, and that is the session again.
func TestSessionPanelPlan_ThreadsBlockReturnsOnLeavingTheSubAgent(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.threadsDock.cache.value = mkDockThreads(3)
	u.sess.navStack = append(u.sess.navStack, sessionNavFrame{})
	require.Empty(t, u.sessionPanelPlan(100).threads)

	u.sess.navStack = nil
	require.Len(t, u.sessionPanelPlan(100).threads, 3)
}

// The header badge and the refreshes behind it are deliberately not gated
// this way: threads keep running while a transcript is being read, and the
// badge is the only sign of it.
func TestSurfacesThreads_StaysOnInASubAgentTranscript(t *testing.T) {
	t.Parallel()

	u := sessionUI()
	u.sess.navStack = append(u.sess.navStack, sessionNavFrame{})

	require.True(t, u.surfacesThreads(), "live thread status is still this workspace's business")
	require.False(t, u.panelSurfacesThreads())
}
