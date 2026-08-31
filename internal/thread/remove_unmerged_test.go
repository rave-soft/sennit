package thread_test

import (
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

// TestManager_RemoveUnmergedBranchWithoutForcePreservesThread proves that an
// unmerged branch is rejected before any destructive teardown. In particular,
// a failed non-force delete must leave its checkout, ref, and store row usable.
func TestManager_RemoveUnmergedBranchWithoutForcePreservesThread(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:        "unmerged",
		Goal:        "",
		MergePolicy: thread.MergeManual,
	})
	require.NoError(t, err)
	writeFile(t, st.WorktreePath, "unmerged.txt", "keep me\n")
	runGit(t, st.WorktreePath, "add", "unmerged.txt")
	runGit(t, st.WorktreePath, "commit", "-m", "unmerged work")

	err = mgr.Remove(t.Context(), st.ID, false, true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not merged")
	require.True(t, worktreeExists(t, st.WorktreePath))
	require.NotEmpty(t, runGit(t, repo, "branch", "--list", st.Branch))

	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, st.ID, got.ID)
	require.FileExists(t, filepath.Join(got.WorktreePath, "unmerged.txt"))
}
