package thread_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/thread"
)

// TestManager_SendRefusesWhenWorktreeIsGone is the regression test for
// G11: Activate checks os.Stat(st.WorktreePath) before respawning
// (manager.go), but Manager.send did not, and lifecycle.send can't do it
// either — it is shared with TaskManager.Send, whose spawnPath is always
// empty. A thread whose worktree was already removed by hand (or by the
// unwinder after a failed create) used to sail straight through to
// Bootstrap, which deliberately accepts a missing path and MkdirAll's a
// fresh, empty, non-git directory — resurrecting a dead thread's agent
// into a directory it has no business running in.
func TestManager_SendRefusesWhenWorktreeIsGone(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "gone", Goal: "do it", MergePolicy: thread.MergeManual})
	require.NoError(t, err)

	// Complete the run so the runtime is released and Send has to respawn
	// from st.WorktreePath rather than dispatch into a live one.
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))

	// The worktree is gone from disk - e.g. removed by hand, or by the
	// unwinder after an earlier failCreate - but the row itself is
	// untouched, so it still resolves and still reports its old path.
	runGit(t, repo, "worktree", "remove", "--force", st.WorktreePath)
	require.False(t, worktreeExists(t, st.WorktreePath))

	spawnsBefore := spawner.spawns()

	_, err = mgr.Send(t.Context(), st.ID, "keep going")
	require.Error(t, err, "Send must refuse to resume a thread whose worktree is gone")
	require.Contains(t, err.Error(), "unavailable")

	require.Equal(t, spawnsBefore, spawner.spawns(),
		"a refused Send must not have spawned a new workspace in a resurrected directory")
}
