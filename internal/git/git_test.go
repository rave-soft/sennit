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

func TestCanonicalCommonDir_Relative(t *testing.T) {
	workingDir := t.TempDir()
	gitDir := filepath.Join(workingDir, ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o755))

	got, err := canonicalCommonDir(filepath.Join(workingDir, "subdir"), "../.git")
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(gitDir)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestCommonDir_NotRepositorySentinel(t *testing.T) {
	requireGit(t)
	_, err := CommonDir(t.Context(), t.TempDir())
	require.ErrorIs(t, err, ErrNotRepository)
}

func TestCommonDir_CommandUnavailableIsNotNotRepository(t *testing.T) {
	oldCommandContext := commandContext
	commandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, filepath.Join(t.TempDir(), "missing-git"), args...)
	}
	t.Cleanup(func() { commandContext = oldCommandContext })

	_, err := CommonDir(t.Context(), t.TempDir())
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotRepository)
}

func TestCommonDir(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	sub := filepath.Join(repo, "sub")
	require.NoError(t, os.MkdirAll(sub, 0o755))

	commonDir, err := CommonDir(ctx, sub)
	require.NoError(t, err)

	want, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	require.NoError(t, err)
	require.Equal(t, want, commonDir)

	worktree := filepath.Join(t.TempDir(), "worktree")
	require.NoError(t, WorktreeAdd(ctx, repo, worktree, "worktree", "main"))
	worktreeCommonDir, err := CommonDir(ctx, worktree)
	require.NoError(t, err)
	require.Equal(t, commonDir, worktreeCommonDir)
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

func TestUncommittedFiles(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	writeFile(t, repo, "README.md", "hello\nworld\n")
	writeFile(t, repo, "untracked.txt", "one\ntwo")

	files, err := UncommittedFiles(ctx, repo)
	require.NoError(t, err)
	require.Equal(t, []FileChange{
		{Path: filepath.Join(repo, "README.md"), Additions: 1},
		{Path: filepath.Join(repo, "untracked.txt"), Additions: 2},
	}, files)
}

func TestUncommittedFilesOutsideRepo(t *testing.T) {
	files, err := UncommittedFiles(t.Context(), t.TempDir())
	require.NoError(t, err)
	require.Nil(t, files)
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

// readOnlyCacheDir plants the shape a build cache leaves inside a worktree:
// a directory git may not write to, holding a file it must unlink to remove
// the worktree. Go's module cache does exactly this to every package
// directory it writes.
func readOnlyCacheDir(t *testing.T, wtPath string) {
	t.Helper()

	cache := filepath.Join(wtPath, ".cache", "go-mod", "example.com", "pkg@v1.0.0")
	require.NoError(t, os.MkdirAll(cache, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cache, "pkg.go"), []byte("package pkg\n"), 0o444))
	require.NoError(t, os.Chmod(cache, 0o555))
	// Restore write permission unconditionally, so a failing test leaves
	// nothing t.TempDir's own cleanup would then choke on.
	t.Cleanup(func() { _ = os.Chmod(cache, 0o755) })
}

// A forced remove has to survive read-only directories. It gets a single
// attempt: git deregisters the worktree before it deletes the files, so a
// failure here is not something a retry can repair — see WorktreeRemove.
func TestWorktreeRemoveForcedThroughReadOnlyDir(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	wtPath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, WorktreeAdd(ctx, repo, wtPath, "feature", "main"))
	readOnlyCacheDir(t, wtPath)

	require.NoError(t, WorktreeRemove(ctx, repo, wtPath, true))
	require.NoDirExists(t, wtPath)

	// The worktree is gone from git's view too, not merely off disk.
	list, err := run(ctx, repo, "worktree", "list", "--porcelain")
	require.NoError(t, err)
	require.NotContains(t, list, wtPath)
}

// An unforced remove keeps refusing a dirty worktree. The permission fix
// belongs to force alone, and must not become a way to discard work.
func TestWorktreeRemoveUnforcedStillRefusesDirtyWorktree(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	wtPath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, WorktreeAdd(ctx, repo, wtPath, "feature", "main"))
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("edited\n"), 0o644))

	require.Error(t, WorktreeRemove(ctx, repo, wtPath, false))
	require.DirExists(t, wtPath)
}

// A worktree an older build left deregistered on disk is cleaned up rather
// than leaked: no git command can reach it, so nothing else ever will.
func TestWorktreeRemoveCleansUpDeregisteredWorktree(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	wtPath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, WorktreeAdd(ctx, repo, wtPath, "feature", "main"))

	// Reproduce the corpse exactly: the administrative directory gone,
	// the working files and their dangling .git pointer still there.
	adminDir, err := run(ctx, wtPath, "rev-parse", "--path-format=absolute", "--git-dir")
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(adminDir))
	readOnlyCacheDir(t, wtPath)
	require.Error(t, func() error {
		_, err := run(ctx, repo, "worktree", "remove", "--force", wtPath)
		return err
	}(), "precondition: git itself can no longer remove this")

	require.NoError(t, WorktreeRemove(ctx, repo, wtPath, true))
	require.NoDirExists(t, wtPath)
}

// Removing a worktree someone already deleted by hand succeeds and leaves
// git's registration pruned. A caller that cannot get past this has no way
// left to finish the cleanup, and its own record of the worktree leaks.
func TestWorktreeRemoveAlreadyGone(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	wtPath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, WorktreeAdd(ctx, repo, wtPath, "feature", "main"))
	require.NoError(t, os.RemoveAll(wtPath))

	for _, force := range []bool{false, true} {
		require.NoError(t, WorktreeRemove(ctx, repo, wtPath, force))
	}

	list, err := run(ctx, repo, "worktree", "list", "--porcelain")
	require.NoError(t, err)
	require.NotContains(t, list, wtPath)
}

// The invariant that keeps a worktree from leaking: while its files are
// still on disk, its registration stays. A container that wrote into a
// worktree owns directories this process cannot chmod, so the removal can
// fail for reasons no retry here can fix — and the retry has to remain
// possible for whenever it can. Deleting the files before git touches its
// metadata is what buys that; letting git go first would deregister the
// worktree and strand it.
func TestWorktreeRemoveForcedKeepsRegistrationWhenFilesSurvive(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	parent := t.TempDir()
	wtPath := filepath.Join(parent, "wt")
	require.NoError(t, WorktreeAdd(ctx, repo, wtPath, "feature", "main"))
	// Stand in for the directories this process may not chmod: the walk
	// inside WorktreeRemove only reaches into the worktree, so a parent it
	// may not write to fails the final unlink of the worktree itself.
	require.NoError(t, os.Chmod(parent, 0o500))
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	require.Error(t, WorktreeRemove(ctx, repo, wtPath, true))

	list, err := run(ctx, repo, "worktree", "list", "--porcelain")
	require.NoError(t, err)
	require.Contains(t, list, wtPath, "the worktree must still be git's to remove")

	// And once whatever held the files lets go, the retry goes through.
	require.NoError(t, os.Chmod(parent, 0o755))
	require.NoError(t, WorktreeRemove(ctx, repo, wtPath, true))
	require.NoDirExists(t, wtPath)
}

// The cleanup path is narrow on purpose: it deletes a directory outright,
// so anything that is not this repo's own abandoned worktree is left alone
// and git's error is reported instead.
func TestWorktreeRemoveLeavesForeignDirectoriesAlone(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	t.Run("plain directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "not-a-worktree")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("mine\n"), 0o644))

		require.Error(t, WorktreeRemove(ctx, repo, dir, true))
		require.FileExists(t, filepath.Join(dir, "keep.txt"))
	})

	t.Run("independent repository", func(t *testing.T) {
		other := initRepo(t)

		require.Error(t, WorktreeRemove(ctx, repo, other, true))
		require.DirExists(t, filepath.Join(other, ".git"))
	})

	t.Run("worktree of another repository", func(t *testing.T) {
		other := initRepo(t)
		wtPath := filepath.Join(t.TempDir(), "other-wt")
		require.NoError(t, WorktreeAdd(ctx, other, wtPath, "feature", "main"))
		adminDir, err := run(ctx, wtPath, "rev-parse", "--path-format=absolute", "--git-dir")
		require.NoError(t, err)
		require.NoError(t, os.RemoveAll(adminDir))

		require.Error(t, WorktreeRemove(ctx, repo, wtPath, true))
		require.DirExists(t, wtPath)
	})
}

func TestDeleteBranch(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	wtPath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, WorktreeAdd(ctx, repo, wtPath, "feature", "main"))
	require.NoError(t, WorktreeRemove(ctx, repo, wtPath, true))

	require.NoError(t, DeleteBranch(ctx, repo, "feature", true))
	exists, err := BranchExists(ctx, repo, "feature")
	require.NoError(t, err)
	require.False(t, exists)

	// A branch already gone is the outcome the caller asked for.
	require.NoError(t, DeleteBranch(ctx, repo, "feature", true))
	require.NoError(t, DeleteBranch(ctx, repo, "never-existed", false))

	// Real refusals still surface: git will not delete a checked-out branch.
	require.Error(t, DeleteBranch(ctx, repo, "main", true))
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
