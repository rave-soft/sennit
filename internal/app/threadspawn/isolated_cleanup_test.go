package threadspawn

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/git"
	"github.com/stretchr/testify/require"
)

func TestIsolatedCleanupPreservesChangesAndUniqueCommits(t *testing.T) {
	for _, mode := range []string{"clean", "untracked", "tracked", "committed", "missing-base"} {
		t.Run(mode, func(t *testing.T) {
			repo := initRepo(t)
			path := filepath.Join(t.TempDir(), "worktree")
			branch := "thread/cleanup"
			require.NoError(t, git.WorktreeAdd(t.Context(), repo, path, branch, "main"))
			base := "main"
			switch mode {
			case "untracked", "committed":
				require.NoError(t, os.WriteFile(filepath.Join(path, "result.txt"), []byte("valuable result"), 0o644))
				if mode == "committed" {
					runGit(t, path, "add", "result.txt")
					runGit(t, path, "commit", "-m", "test result")
				}
			case "tracked":
				require.NoError(t, os.WriteFile(filepath.Join(path, "README.md"), []byte("valuable edit"), 0o644))
			case "missing-base":
				base = "missing-base"
			}
			err := cleanupIsolatedTask(t.Context(), repo, path, branch, base)
			exists, branchErr := git.BranchExists(t.Context(), repo, branch)
			require.NoError(t, branchErr)
			_, pathErr := os.Stat(path)
			if mode == "clean" {
				require.NoError(t, err)
				require.False(t, exists)
				require.True(t, os.IsNotExist(pathErr))
			} else {
				require.Error(t, err)
				require.True(t, exists)
				require.NoError(t, pathErr)
			}
		})
	}
}
