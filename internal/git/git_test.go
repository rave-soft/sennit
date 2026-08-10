package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

// initRepo creates a scratch git repo in a fresh temp dir, with local
// user.email/user.name config and one commit on "main" so branches have a
// base to fork from.
func initRepo(t *testing.T) string {
	t.Helper()
	requireGit(t)

	dir := t.TempDir()
	ctx := context.Background()

	_, err := run(ctx, dir, "init", "-b", "main")
	require.NoError(t, err)
	_, err = run(ctx, dir, "config", "user.email", "test@example.com")
	require.NoError(t, err)
	_, err = run(ctx, dir, "config", "user.name", "Test")
	require.NoError(t, err)

	writeFile(t, dir, "README.md", "hello\n")
	_, err = run(ctx, dir, "add", "-A")
	require.NoError(t, err)
	_, err = run(ctx, dir, "commit", "-m", "initial commit")
	require.NoError(t, err)

	return dir
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}

func TestIsRepo(t *testing.T) {
	requireGit(t)
	repo := initRepo(t)
	ctx := context.Background()

	require.True(t, IsRepo(ctx, repo))
	require.False(t, IsRepo(ctx, t.TempDir()))
}

func TestTopLevel(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	sub := filepath.Join(repo, "sub")
	require.NoError(t, os.MkdirAll(sub, 0o755))

	top, err := TopLevel(ctx, sub)
	require.NoError(t, err)

	// Resolve symlinks (e.g. macOS /tmp -> /private/tmp) before comparing.
	wantTop, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	gotTop, err := filepath.EvalSymlinks(top)
	require.NoError(t, err)
	require.Equal(t, wantTop, gotTop)
}

func TestCurrentBranch(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	branch, err := CurrentBranch(ctx, repo)
	require.NoError(t, err)
	require.Equal(t, "main", branch)
}

func TestIsDirty(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	dirty, err := IsDirty(ctx, repo)
	require.NoError(t, err)
	require.False(t, dirty)

	writeFile(t, repo, "untracked.txt", "x\n")

	dirty, err = IsDirty(ctx, repo)
	require.NoError(t, err)
	require.True(t, dirty)
}

func TestBranchExists(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	exists, err := BranchExists(ctx, repo, "main")
	require.NoError(t, err)
	require.True(t, exists)

	exists, err = BranchExists(ctx, repo, "does-not-exist")
	require.NoError(t, err)
	require.False(t, exists)
}

func TestWorktreeAdd(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	wtPath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, WorktreeAdd(ctx, repo, wtPath, "feature", "main"))

	require.DirExists(t, wtPath)

	exists, err := BranchExists(ctx, repo, "feature")
	require.NoError(t, err)
	require.True(t, exists)

	branch, err := CurrentBranch(ctx, wtPath)
	require.NoError(t, err)
	require.Equal(t, "feature", branch)
}

func TestWorktreeRemove(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	wtPath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, WorktreeAdd(ctx, repo, wtPath, "feature", "main"))
	require.NoError(t, WorktreeRemove(ctx, repo, wtPath, false))

	require.NoDirExists(t, wtPath)
}

func TestCommitAll(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	// Nothing to commit: clean worktree.
	committed, err := CommitAll(ctx, repo, "no-op")
	require.NoError(t, err)
	require.False(t, committed)

	// Dirty worktree: commit succeeds.
	writeFile(t, repo, "new.txt", "content\n")
	committed, err = CommitAll(ctx, repo, "add new.txt")
	require.NoError(t, err)
	require.True(t, committed)

	dirty, err := IsDirty(ctx, repo)
	require.NoError(t, err)
	require.False(t, dirty)
}

func TestMergeIntoWorktreeClean(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	wtPath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, WorktreeAdd(ctx, repo, wtPath, "feature", "main"))

	writeFile(t, wtPath, "feature.txt", "feature content\n")
	committed, err := CommitAll(ctx, wtPath, "add feature.txt")
	require.NoError(t, err)
	require.True(t, committed)

	// Merge feature into main, checked out in repo's primary worktree.
	result, err := MergeIntoWorktree(ctx, repo, "feature")
	require.NoError(t, err)
	require.True(t, result.Merged)
	require.Empty(t, result.Conflicts)
	require.FileExists(t, filepath.Join(repo, "feature.txt"))
}

func TestMergeIntoWorktreeConflict(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	// Both branches edit the same line in README.md, guaranteeing a
	// conflict on merge.
	writeFile(t, repo, "README.md", "main version\n")
	committed, err := CommitAll(ctx, repo, "edit on main")
	require.NoError(t, err)
	require.True(t, committed)

	wtPath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, WorktreeAdd(ctx, repo, wtPath, "feature", "main"))
	// Reset feature branch to before main's edit so it diverges cleanly.
	_, err = run(ctx, wtPath, "reset", "--hard", "HEAD~1")
	require.NoError(t, err)
	writeFile(t, wtPath, "README.md", "feature version\n")
	committed, err = CommitAll(ctx, wtPath, "edit on feature")
	require.NoError(t, err)
	require.True(t, committed)

	result, err := MergeIntoWorktree(ctx, repo, "feature")
	require.NoError(t, err)
	require.False(t, result.Merged)
	require.Equal(t, []string{"README.md"}, result.Conflicts)

	// Clean up so other tests in the same repo (none currently, but future
	// callers) aren't left with a merge in progress.
	require.NoError(t, AbortMerge(ctx, repo))

	dirty, err := IsDirty(ctx, repo)
	require.NoError(t, err)
	require.False(t, dirty)
}

func TestFastForward(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	// base3 is a third branch not checked out anywhere, used as the
	// fast-forward target so the "success" case doesn't collide with the
	// "checked out" case below.
	_, err := run(ctx, repo, "branch", "base3")
	require.NoError(t, err)

	wtPath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, WorktreeAdd(ctx, repo, wtPath, "feature", "main"))
	writeFile(t, wtPath, "feature.txt", "content\n")
	committed, err := CommitAll(ctx, wtPath, "add feature.txt")
	require.NoError(t, err)
	require.True(t, committed)

	require.NoError(t, FastForward(ctx, repo, "feature", "base3"))

	tip, err := run(ctx, repo, "rev-parse", "base3")
	require.NoError(t, err)
	featureTip, err := run(ctx, repo, "rev-parse", "feature")
	require.NoError(t, err)
	require.Equal(t, featureTip, tip)
}

func TestFastForwardBranchCheckedOut(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	wtPath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, WorktreeAdd(ctx, repo, wtPath, "feature", "main"))
	writeFile(t, wtPath, "feature.txt", "content\n")
	committed, err := CommitAll(ctx, wtPath, "add feature.txt")
	require.NoError(t, err)
	require.True(t, committed)

	// "main" is checked out in repo's own primary worktree, so updating it
	// via push must be refused.
	err = FastForward(ctx, repo, "feature", "main")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrBranchCheckedOut))
}

func TestFastForwardNonFastForward(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	_, err := run(ctx, repo, "branch", "base3")
	require.NoError(t, err)

	wtPath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, WorktreeAdd(ctx, repo, wtPath, "feature", "main"))
	writeFile(t, wtPath, "feature.txt", "content\n")
	committed, err := CommitAll(ctx, wtPath, "add feature.txt")
	require.NoError(t, err)
	require.True(t, committed)

	// Advance base3 independently so feature is no longer an ancestor of
	// its history, forcing a non-fast-forward push.
	writeFile(t, repo, "base3-only.txt", "x\n")
	_, err = run(ctx, repo, "checkout", "base3")
	require.NoError(t, err)
	committed, err = CommitAll(ctx, repo, "diverge base3")
	require.NoError(t, err)
	require.True(t, committed)
	_, err = run(ctx, repo, "checkout", "main")
	require.NoError(t, err)

	err = FastForward(ctx, repo, "feature", "base3")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNonFastForward))
}
