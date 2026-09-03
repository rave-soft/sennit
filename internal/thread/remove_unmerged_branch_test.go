package thread_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/thread"
)

// TestManager_RemoveRefusesUnmergedBranchWithoutTearingDownTheRuntime is the
// regression test for G10: an unforced delete_branch on an unmerged thread
// must refuse before it touches the live runtime, not after. Remove used to
// mark the entity removed, null out its runtime, and shut its App down
// (releaseRuntime) before ever checking git.IsAncestor — so a refusal here
// still left the row "idle" (the type's own "workspace is live" contract)
// while the App backing the person's screen was already dead: the screen
// stopped updating, and the next input would have spawned a new App whose
// events it could never see.
func TestManager_RemoveRefusesUnmergedBranchWithoutTearingDownTheRuntime(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	// An idle thread (no Goal) keeps its workspace live per Create's own
	// doc comment - exactly the "attached screen" case this bug destroyed.
	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:        "stuck",
		MergePolicy: thread.MergeManual,
	})
	require.NoError(t, err)
	require.Equal(t, thread.StatusIdle, st.Status)
	writeFile(t, st.WorktreePath, "unmerged.txt", "keep me\n")
	runGit(t, st.WorktreePath, "add", "unmerged.txt")
	runGit(t, st.WorktreePath, "commit", "-m", "unmerged work")

	// This thread's branch carries a commit its base doesn't have, so an
	// unforced delete_branch must refuse.
	err = mgr.Remove(t.Context(), st.ID, false, true)
	require.Error(t, err, "an unmerged branch must refuse delete_branch without force")
	require.Contains(t, err.Error(), "not merged")

	// The runtime must still be live: the refusal happened before
	// teardown, not after.
	require.NotNil(t, mgr.Handle(st.ID), "a refused Remove must leave the runtime live")
	require.False(t, spawner.wasReleased(st.WorktreePath), "a refused Remove must not have released the workspace")

	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusIdle, got.Status, "a refused Remove must leave the row idle, not removed")

	// The entity must not be wedged as "removed" either - see
	// TestManager_RemoveThatFailsLeavesTheThreadUsable's identical check.
	_, err = mgr.Send(t.Context(), st.ID, "still here?")
	if err != nil {
		require.NotContains(t, err.Error(), "has been removed",
			"a thread whose delete_branch refusal happened before teardown must not be wedged as removed")
	}
}
