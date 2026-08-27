package thread_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/thread"
)

// TestManager_AutoMergeCompletesOnACanceledFollowUpContext pins onAutoMerge
// against a shutdown racing a thread's run completion. handleRunComplete
// nils the runtime and hands onAutoMerge the manager's own long-lived
// context (followUpCtx) so its merge goroutine can outlive the call that
// started it - but that same context is exactly what Shutdown cancels
// (m.cancel), so an auto-merge landing during shutdown used to run
// mergeAttempt on an already-canceled context, fail silently, and strand
// the row at StatusRunning until the next process's Recover. onAutoMerge
// must detach its own working context so the merge (and the delivery/
// discard that follow it) still lands.
func TestManager_AutoMergeCompletesOnACanceledFollowUpContext(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, parentApp := newTestManagerWithParentApp(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:            "epsilon",
		Goal:            "do it",
		ParentSessionID: "parent-sess",
	})
	require.NoError(t, err)
	require.Equal(t, thread.MergeAuto, st.MergePolicy)

	writeFile(t, st.WorktreePath, "output.txt", "auto merged\n")

	runID, live := mgr.RuntimeForTest(st.ID)
	require.True(t, live, "the thread must own a live runtime to complete against")

	// The shape a shutdown racing this completion arrives in: the
	// manager's own long-lived context, already canceled by the time the
	// merge goroutine onAutoMerge starts would read it.
	dead, cancel := context.WithCancel(t.Context())
	cancel()

	_ = spawner.appFor(st.WorktreePath)
	mgr.HandleRunCompleteForTest(dead, st.ID, thread.RunComplete{
		SessionID: st.SessionID,
		RunID:     runID,
		Text:      "finished",
	})

	require.Eventually(t, func() bool {
		got, err := mgr.Get(t.Context(), st.ID)
		return err == nil && got.Status == thread.StatusMerged
	}, eventuallyTimeout, eventuallyTick,
		"the merge must still land even though the follow-up context was already canceled")

	parentCoord := parentApp.Coordinator().(*fakeCoordinator)
	require.Eventually(t, func() bool { return len(parentCoord.deliveredCompletions()) > 0 }, eventuallyTimeout, eventuallyTick)
	delivered := parentCoord.deliveredCompletions()
	require.Len(t, delivered, 1)
	require.Equal(t, string(thread.StatusMerged), delivered[0].completion.Status)
}
