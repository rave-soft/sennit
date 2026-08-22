//go:build !windows

package git

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// blockRemoval provokes a worktree-removal failure the way it actually
// happens on POSIX: strip write permission from wtPath's parent, so
// unlinking the worktree directory's own entry fails even though
// WorktreeRemove's makeDirsWritable has already made everything *inside*
// wtPath removable. Calling the returned func restores permissions; it is
// safe to call more than once (chmod is idempotent).
func blockRemoval(t *testing.T, parent, wtPath string) func() {
	t.Helper()
	require.NoError(t, os.Chmod(parent, 0o500))
	return func() { _ = os.Chmod(parent, 0o755) }
}
