package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/fsext"
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
	require.Len(t, files, 2)

	// t.TempDir() and the git subprocess can spell the same directory
	// differently on Windows (an 8.3 short name vs. the long form), so
	// canonicalize both sides before comparing paths byte-for-byte.
	for i := range files {
		files[i].Path = fsext.Canonical(files[i].Path)
	}
	require.Equal(t, []FileChange{
		{Path: fsext.Canonical(filepath.Join(repo, "README.md")), Additions: 1},
		{Path: fsext.Canonical(filepath.Join(repo, "untracked.txt")), Additions: 2},
	}, files)
}

// TestUncommittedFiles_UnbornHEAD guards a repo with no commits yet: HEAD
// doesn't resolve to anything, so `git diff HEAD` fails outright and used
// to make the whole call error out instead of just reporting every file as
// new (which, on an unborn HEAD, all of them necessarily are).
func TestUncommittedFiles_UnbornHEAD(t *testing.T) {
	requireGit(t)
	repo := t.TempDir()
	ctx := context.Background()

	_, err := run(ctx, repo, "init", "-b", "main")
	require.NoError(t, err)
	_, err = run(ctx, repo, "config", "user.email", "test@example.com")
	require.NoError(t, err)
	_, err = run(ctx, repo, "config", "user.name", "Test")
	require.NoError(t, err)

	writeFile(t, repo, "README.md", "hello\nworld\n")
	_, err = run(ctx, repo, "add", "-A")
	require.NoError(t, err)

	files, err := UncommittedFiles(ctx, repo)
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, fsext.Canonical(filepath.Join(repo, "README.md")), fsext.Canonical(files[0].Path))
	require.Equal(t, 2, files[0].Additions)
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

// A repo-wide "git worktree prune" after removing one worktree must not
// sweep up a different, unrelated worktree that a partial removal left
// prunable-but-populated: its .git pointer file gone (os.RemoveAll deletes
// entries in sorted order, and ".git" sorts first) while its other files
// are still on disk. Deregistering it there would make those files
// unreclaimable, since ownedLinkedWorktree relies on the registration to
// decide what Sennit may touch. See TECHDEBT.md's former entry on this.
//
// All three of WorktreeRemove's exit paths are covered, because all three
// used to end in the same unscoped prune call: a plain "git worktree
// remove" success, the os.Lstat "already gone" branch, and the forced
// self-RemoveAll branch. Each case also pins the positive half of the
// contract -- the target itself must actually end up deregistered -- so a
// deregisterMissingWorktree that silently did nothing would not pass either.
func TestWorktreeRemoveDeregistersOnlyItsOwnTarget(t *testing.T) {
	tests := []struct {
		name string
		// remove exercises one of WorktreeRemove's three success paths
		// against targetPath, which is a freshly added, healthy worktree.
		remove func(t *testing.T, ctx context.Context, repo, targetPath string)
	}{
		{
			name: "plain git worktree remove",
			remove: func(t *testing.T, ctx context.Context, repo, targetPath string) {
				t.Helper()
				require.NoError(t, WorktreeRemove(ctx, repo, targetPath, false))
			},
		},
		{
			name: "already gone by hand",
			remove: func(t *testing.T, ctx context.Context, repo, targetPath string) {
				t.Helper()
				// Simulate a caller cleaning up after someone deleted the
				// worktree by hand: nothing left on disk to Lstat.
				require.NoError(t, os.RemoveAll(targetPath))
				require.NoError(t, WorktreeRemove(ctx, repo, targetPath, false))
			},
		},
		{
			name: "forced self-RemoveAll",
			remove: func(t *testing.T, ctx context.Context, repo, targetPath string) {
				t.Helper()
				require.NoError(t, WorktreeRemove(ctx, repo, targetPath, true))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := initRepo(t)
			ctx := context.Background()

			damagedPath := filepath.Join(t.TempDir(), "damaged")
			require.NoError(t, WorktreeAdd(ctx, repo, damagedPath, "damaged-branch", "main"))
			targetPath := filepath.Join(t.TempDir(), "target")
			require.NoError(t, WorktreeAdd(ctx, repo, targetPath, "target-branch", "main"))

			// Reproduce exactly what a partial os.RemoveAll leaves behind:
			// the worktree's own .git pointer file gone, everything else
			// untouched.
			require.NoError(t, os.Remove(filepath.Join(damagedPath, ".git")))
			require.FileExists(t, filepath.Join(damagedPath, "README.md"))

			tt.remove(t, ctx, repo, targetPath)

			// Positive: the target is actually deregistered, not just left
			// alone -- a no-op deregisterMissingWorktree must not pass.
			targetReg, err := worktreeRegistration(ctx, repo, targetPath)
			require.NoError(t, err)
			require.False(t, targetReg.registered, "target worktree must be deregistered")

			// Negative: the unrelated, damaged worktree is untouched --
			// its files are still on disk, and losing its registration is
			// what makes them unreclaimable.
			damagedReg, err := worktreeRegistration(ctx, repo, damagedPath)
			require.NoError(t, err)
			require.True(t, damagedReg.registered, "damaged worktree must stay registered while its files survive")
			require.FileExists(t, filepath.Join(damagedPath, "README.md"))
		})
	}
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
	// blockRemoval provokes removal failure the way each platform actually
	// fails it — see its two (POSIX/Windows) implementations for why one
	// mechanism does not serve both.
	unblock := blockRemoval(t, parent, wtPath)
	t.Cleanup(unblock)

	require.Error(t, WorktreeRemove(ctx, repo, wtPath, true))

	// A raw substring check against "worktree list --porcelain" output
	// does not survive round-tripping wtPath through git: git reports
	// paths with forward slashes regardless of OS, and on Windows
	// t.TempDir() hands back an 8.3 short name while git resolves to the
	// long form. worktreeRegistration is the same comparison production
	// code already relies on (canonicalPath resolves both spellings to
	// one), so use it here instead of re-deriving the normalization.
	reg, err := worktreeRegistration(ctx, repo, wtPath)
	require.NoError(t, err)
	require.True(t, reg.registered, "the worktree must still be git's to remove")

	// And once whatever held the files lets go, the retry goes through.
	unblock()
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

// TestCanonicalPath_SymlinkedAliasMissingLeaf pins the mechanism behind
// the macOS/Windows regression in deregisterMissingWorktree: it runs only
// once the worktree is already gone from disk, so filepath.EvalSymlinks
// can never resolve the leaf, and canonicalPath must still resolve the
// aliased parent (macOS's /var -> /private/var, a Windows 8.3 short name)
// so two spellings of the same missing path compare equal.
func TestCanonicalPath_SymlinkedAliasMissingLeaf(t *testing.T) {
	real := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(real, alias))

	viaAlias := filepath.Join(alias, "wt")
	viaReal := filepath.Join(real, "wt")
	_, err := os.Lstat(viaAlias)
	require.True(t, os.IsNotExist(err), "precondition: the leaf must not exist")

	require.Equal(t, canonicalPath(viaReal), canonicalPath(viaAlias))
}

// TestWorktreeRemoveDeregistersAliasedMissingWorktree reproduces the
// regression from commit e78e6119: replacing a repo-wide "git worktree
// prune" with deregisterMissingWorktree made deregistration depend on
// worktreeRegistration recognizing the vanished path, which in turn
// depends on canonicalPath resolving it the same way git itself does. Git
// registers worktree paths resolved (worktree add itself calls realpath),
// so a worktree added through a symlinked parent and then deleted by hand
// exercises exactly the aliasing class that is invisible on a plain temp
// dir: this is the same bug macOS's /var -> /private/var symlink and
// Windows' 8.3 short names trigger on those platforms, reproduced here
// with a symlink so it also fails on Linux.
func TestWorktreeRemoveDeregistersAliasedMissingWorktree(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	real := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(real, alias))

	wtPath := filepath.Join(alias, "wt")
	require.NoError(t, WorktreeAdd(ctx, repo, wtPath, "feature", "main"))
	require.NoError(t, os.RemoveAll(wtPath))

	require.NoError(t, WorktreeRemove(ctx, repo, wtPath, false))

	reg, err := worktreeRegistration(ctx, repo, wtPath)
	require.NoError(t, err)
	require.False(t, reg.registered, "the aliased, now-missing worktree must be deregistered")

	// Matches the user-visible symptom in the bug report: a stale
	// registration keeps the branch pinned and DeleteBranch fails.
	require.NoError(t, DeleteBranch(ctx, repo, "feature", true))
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

// createRefAt points refs/heads/name at repo's current HEAD via update-ref,
// bypassing "git branch"'s own name validation. Git's ref-format rules
// permit a leading dash in a ref component (only the porcelain "branch"
// command forbids it — see git-check-ref-format(1)), so a branch named this
// way can genuinely exist; this is how a thread name arriving from outside
// Sennit's own slug validation could reach these functions.
func createRefAt(t *testing.T, ctx context.Context, repo, name string) {
	t.Helper()
	head, err := run(ctx, repo, "rev-parse", "HEAD")
	require.NoError(t, err)
	_, err = run(ctx, repo, "update-ref", "refs/heads/"+name, head)
	require.NoError(t, err)
}

// A branch name beginning with "-" must be handled as a name, not parsed as
// a git option. Before the fix, "worktree add -b -f path base" had "-f"
// consumed as --force by worktree add's internal branch-creation step,
// which then tried to force-update whatever base pointed at (see the
// WorktreeAdd doc comment) instead of failing on an invalid branch name.
func TestWorktreeAdd_LeadingDashBranchNameFailsClosed(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	// "victim" is not checked out anywhere, so nothing here stops
	// "-D" from being honored as --D (force-delete) against it if it
	// gets parsed as an option instead of a branch name — see the doc
	// comment on WorktreeAdd. Before the fix, "git worktree add -b -D
	// path victim" deleted "victim" outright and only then failed on
	// checkout, so the branch was already gone by the time the error
	// came back.
	_, err := run(ctx, repo, "branch", "victim")
	require.NoError(t, err)

	wtPath := filepath.Join(t.TempDir(), "wt")
	err = WorktreeAdd(ctx, repo, wtPath, "-D", "victim")
	require.Error(t, err)

	exists, err := BranchExists(ctx, repo, "victim")
	require.NoError(t, err)
	require.True(t, exists, "the base branch must survive a rejected branch name")

	require.NoDirExists(t, wtPath)
}

// A base ref beginning with "-" is treated as a name to check out from, not
// as an option to "git branch"/"git worktree add".
func TestWorktreeAdd_LeadingDashBase(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	createRefAt(t, ctx, repo, "-based")

	wtPath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, WorktreeAdd(ctx, repo, wtPath, "feature", "-based"))

	tip, err := run(ctx, repo, "rev-parse", "refs/heads/-based")
	require.NoError(t, err)
	featureTip, err := run(ctx, wtPath, "rev-parse", "HEAD")
	require.NoError(t, err)
	require.Equal(t, tip, featureTip)
}

// WorktreeAdd is two git calls, not one: "git branch -- newBranch base"
// followed by "git worktree add -- path newBranch" (see its doc comment for
// why the atomic "-b" form can't be used). That opens a window a single
// command never had: the branch call can succeed and the worktree call can
// still fail, e.g. because path is already occupied. Without cleanup that
// leaves newBranch behind, and thread creation's only real caller reuses
// the same name on retry, so the branch would keep "already exists"-ing
// forever.
func TestWorktreeAdd_CleansUpBranchOnFailedCheckout(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	// A pre-existing, non-empty directory at path makes "worktree add"
	// fail after "branch --" has already created the branch.
	occupied := filepath.Join(t.TempDir(), "occupied")
	require.NoError(t, os.MkdirAll(occupied, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(occupied, "x"), []byte("x"), 0o644))

	err := WorktreeAdd(ctx, repo, occupied, "newbranch", "main")
	require.Error(t, err)

	exists, err := BranchExists(ctx, repo, "newbranch")
	require.NoError(t, err)
	require.False(t, exists, "a failed worktree add must not leave the branch behind")
}

// A worktree path beginning with "-" is removed as a path, not parsed as an
// option to "git worktree remove".
func TestWorktreeRemove_LeadingDashPath(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	wtPath := filepath.Join(t.TempDir(), "-weird")
	require.NoError(t, WorktreeAdd(ctx, repo, wtPath, "feature", "main"))
	require.NoError(t, WorktreeRemove(ctx, repo, wtPath, false))

	require.NoDirExists(t, wtPath)
}

// A branch name beginning with "-" is deleted as a name, not parsed as a
// second flag to "git branch -d/-D".
func TestDeleteBranch_LeadingDashName(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	createRefAt(t, ctx, repo, "-weird")

	require.NoError(t, DeleteBranch(ctx, repo, "-weird", true))

	exists, err := BranchExists(ctx, repo, "-weird")
	require.NoError(t, err)
	require.False(t, exists)

	// "main" (this repo's checked-out branch) must be unaffected.
	branch, err := CurrentBranch(ctx, repo)
	require.NoError(t, err)
	require.Equal(t, "main", branch)
}

// A ref beginning with "-" is looked up as a name in both positions of
// "git merge-base --is-ancestor", not parsed as an option.
func TestIsAncestor_LeadingDashRef(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	createRefAt(t, ctx, repo, "-weird")

	isAncestor, err := IsAncestor(ctx, repo, "-weird", "main")
	require.NoError(t, err)
	require.True(t, isAncestor, "the dash-named ref points at the same commit as main")
}

// A ref beginning with "-" is merged as a name, not parsed as an option to
// "git merge --no-edit".
func TestMergeIntoWorktree_LeadingDashRef(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	// Advance a detached HEAD rather than the "-weird" ref itself (checking
	// it out by its bare name hits the very option-parsing ambiguity this
	// package works around, and is not what's under test here), then point
	// refs/heads/-weird at the new commit directly.
	head, err := run(ctx, repo, "rev-parse", "HEAD")
	require.NoError(t, err)
	_, err = run(ctx, repo, "checkout", "--detach", head)
	require.NoError(t, err)
	writeFile(t, repo, "feature.txt", "content\n")
	committed, err := CommitAll(ctx, repo, "add feature.txt")
	require.NoError(t, err)
	require.True(t, committed)
	newHead, err := run(ctx, repo, "rev-parse", "HEAD")
	require.NoError(t, err)
	_, err = run(ctx, repo, "update-ref", "refs/heads/-weird", newHead)
	require.NoError(t, err)
	_, err = run(ctx, repo, "checkout", "main")
	require.NoError(t, err)

	result, err := MergeIntoWorktree(ctx, repo, "-weird")
	require.NoError(t, err)
	require.True(t, result.Merged)
	require.FileExists(t, filepath.Join(repo, "feature.txt"))
}

// A ref beginning with "-" fast-forwards the checked-out branch as a name,
// not parsed as an option to "git merge --ff-only".
func TestMergeFFOnly_LeadingDashRef(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	// As in TestMergeIntoWorktree_LeadingDashRef, advance a detached HEAD
	// and point refs/heads/-weird at it directly rather than checking out
	// "-weird" by name, so the setup itself doesn't hit the ambiguity this
	// package's fix works around.
	head, err := run(ctx, repo, "rev-parse", "HEAD")
	require.NoError(t, err)
	_, err = run(ctx, repo, "checkout", "--detach", head)
	require.NoError(t, err)
	writeFile(t, repo, "feature.txt", "content\n")
	committed, err := CommitAll(ctx, repo, "add feature.txt")
	require.NoError(t, err)
	require.True(t, committed)
	newHead, err := run(ctx, repo, "rev-parse", "HEAD")
	require.NoError(t, err)
	_, err = run(ctx, repo, "update-ref", "refs/heads/-weird", newHead)
	require.NoError(t, err)
	_, err = run(ctx, repo, "checkout", "main")
	require.NoError(t, err)

	require.NoError(t, MergeFFOnly(ctx, repo, "-weird"))
	require.FileExists(t, filepath.Join(repo, "feature.txt"))
}

// A source branch name beginning with "-" survives FastForward's
// push-based update: the combined "branch:ontoBase" refspec string starts
// with whatever branch starts with, so this is the position that actually
// exercises the option-parsing ambiguity (the target half sits after the
// ":" and never leads the argument).
func TestFastForward_LeadingDashBranch(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	// "-weird" is the fast-forward source, one commit ahead of "base3",
	// neither of them checked out anywhere.
	_, err := run(ctx, repo, "branch", "base3")
	require.NoError(t, err)
	head, err := run(ctx, repo, "rev-parse", "HEAD")
	require.NoError(t, err)
	_, err = run(ctx, repo, "checkout", "--detach", head)
	require.NoError(t, err)
	writeFile(t, repo, "feature.txt", "content\n")
	committed, err := CommitAll(ctx, repo, "add feature.txt")
	require.NoError(t, err)
	require.True(t, committed)
	newHead, err := run(ctx, repo, "rev-parse", "HEAD")
	require.NoError(t, err)
	_, err = run(ctx, repo, "update-ref", "refs/heads/-weird", newHead)
	require.NoError(t, err)
	_, err = run(ctx, repo, "checkout", "main")
	require.NoError(t, err)

	require.NoError(t, FastForward(ctx, repo, "-weird", "base3"))

	tip, err := run(ctx, repo, "rev-parse", "base3")
	require.NoError(t, err)
	require.Equal(t, newHead, tip)
}
