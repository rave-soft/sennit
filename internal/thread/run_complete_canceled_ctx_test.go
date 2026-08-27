package thread_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/thread"
)

// TestManager_RunCompleteRecordsAndDeliversOnACanceledContext pins the
// terminal bookkeeping against the context it is handed. handleRunComplete
// used to do its store reads and writes on the caller's context — the
// run's own, or the manager's long-lived one — and a cancellation is
// precisely what cancels those. A thread whose run ended that way logged
// "Failed to load thread after run completion; status left stale", left
// the row sitting at running, and never reached deliverCompletion, so the
// parent was never told its delegation had ended.
func TestManager_RunCompleteRecordsAndDeliversOnACanceledContext(t *testing.T) {
	repo := initRepo(t)
	mgr, _, parentApp := newTestManagerWithParentApp(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:            "alpha",
		Goal:            "do the thing",
		MergePolicy:     thread.MergeManual,
		ParentSessionID: "parent-sess",
	})
	require.NoError(t, err)

	runID, live := mgr.RuntimeForTest(st.ID)
	require.True(t, live, "the thread must own a live runtime to complete against")

	// The context the run completes on is already dead — the shape every
	// cancellation and shutdown arrives in.
	dead, cancel := context.WithCancel(t.Context())
	cancel()

	mgr.HandleRunCompleteForTest(dead, st.ID, thread.RunComplete{
		SessionID: st.SessionID,
		RunID:     runID,
		Text:      "finished",
	})

	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusCompleted, got.Status,
		"the terminal status must be recorded even though the context was canceled")

	parentCoord := parentApp.Coordinator().(*fakeCoordinator)
	delivered := parentCoord.deliveredCompletions()
	require.Len(t, delivered, 1, "the parent must still be told its delegation ended")
	require.Equal(t, "parent-sess", delivered[0].sessionID)
	require.Equal(t, st.ID, delivered[0].completion.DelegationID)
	require.Equal(t, string(thread.StatusCompleted), delivered[0].completion.Status)
	require.Equal(t, "finished", delivered[0].completion.ResultText)
}
