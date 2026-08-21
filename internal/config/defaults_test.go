package config

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireGit skips the test if git is not on PATH.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
}

// TestApplyEnvironmentDefaults_UsesWorkspaceDirNotProcessCwd pins down that
// isInsideWorktree answers for cfg.workingDir, not for the process's
// current working directory. The test process itself runs from within this
// package's directory, which is inside the sennit git worktree, so the old
// cwd-based check would have reported "inside a worktree" regardless of
// which workspace cfg described — silently skipping the reduced file-walk
// limits for a non-git workspace.
func TestApplyEnvironmentDefaults_UsesWorkspaceDirNotProcessCwd(t *testing.T) {
	requireGit(t)

	repoDir := t.TempDir()
	_, err := exec.CommandContext(t.Context(), "git", "-C", repoDir, "init").CombinedOutput()
	require.NoError(t, err)

	t.Run("inside a git worktree", func(t *testing.T) {
		cfg := &Config{}
		cfg.setDefaults(repoDir, "")

		applyEnvironmentDefaults(cfg)

		require.Nil(t, cfg.Tools.Ls.MaxDepth, "should not clamp file-walk limits inside a git worktree")
		require.Nil(t, cfg.Tools.Ls.MaxItems, "should not clamp file-walk limits inside a git worktree")
		require.Nil(t, cfg.Options.TUI.Completions.MaxDepth)
		require.Nil(t, cfg.Options.TUI.Completions.MaxItems)
	})

	t.Run("outside any git worktree", func(t *testing.T) {
		plainDir := t.TempDir()

		cfg := &Config{}
		cfg.setDefaults(plainDir, "")

		applyEnvironmentDefaults(cfg)

		require.NotNil(t, cfg.Tools.Ls.MaxDepth)
		require.Equal(t, 2, *cfg.Tools.Ls.MaxDepth)
		require.NotNil(t, cfg.Tools.Ls.MaxItems)
		require.Equal(t, 100, *cfg.Tools.Ls.MaxItems)
		require.NotNil(t, cfg.Options.TUI.Completions.MaxDepth)
		require.Equal(t, 2, *cfg.Options.TUI.Completions.MaxDepth)
		require.NotNil(t, cfg.Options.TUI.Completions.MaxItems)
		require.Equal(t, 100, *cfg.Options.TUI.Completions.MaxItems)
	})
}
