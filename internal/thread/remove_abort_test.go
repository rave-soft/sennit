package thread_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/thread"
)

// TestManager_RemoveThatFailsLeavesTheThreadUsable pins the zombie a
// failed removal used to create. Remove marks the entity removed up front
// so a Create racing the teardown cannot resurrect it — but the flag
// stayed set when the teardown then failed, and every later operation
// consults it: the row was still there, still listed, and every call
// against it answered "has been removed" until the process restarted.
func TestManager_RemoveThatFailsLeavesTheThreadUsable(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:        "stuck",
		Goal:        "do it",
		MergePolicy: thread.MergeManual,
	})
	require.NoError(t, err)

	// The same stand-in TestDiscardMerged_KeepsTheRowWhenTheWorktreeCannotGo
	// uses: a worktree this process cannot remove.
	t.Cleanup(blockWorktreeRemoval(t, st.WorktreePath))

	require.Error(t, mgr.Remove(t.Context(), st.ID, true, false),
		"the worktree cannot go, so the removal must fail")

	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err, "the row survives a failed removal")
	require.Equal(t, st.ID, got.ID)

	// The real assertion: the thread is still operable, not permanently
	// "removed". Send resolves through the same flag Get does.
	_, err = mgr.Send(t.Context(), st.ID, "still here?")
	if err != nil {
		require.NotContains(t, err.Error(), "has been removed",
			"a thread whose removal failed must not be wedged as removed")
	}
}
