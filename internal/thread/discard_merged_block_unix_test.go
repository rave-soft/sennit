//go:build !windows

package thread_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// blockWorktreeRemoval provokes a worktree-removal failure the way it
// actually happens on POSIX: strip write permission from the worktree's
// parent directory, so unlinking the worktree directory's own entry fails
// even though [git.WorktreeRemove]'s internal chmod pass has already made
// everything *inside* the worktree removable. Calling the returned func
// restores permissions; it is safe to call more than once.
func blockWorktreeRemoval(t *testing.T, worktreePath string) func() {
	t.Helper()
	parent := filepath.Dir(worktreePath)
	require.NoError(t, os.Chmod(parent, 0o500))
	return func() { _ = os.Chmod(parent, 0o755) }
}
