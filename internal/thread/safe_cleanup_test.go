package thread_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

func pathExistsForTest(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func waitForThreadCleanup(t *testing.T, mgr *thread.Manager, id string) (thread.Thread, bool) {
	t.Helper()
	var current thread.Thread
	var retained bool
	require.Eventually(t, func() bool {
		got, err := mgr.Get(context.Background(), id)
		if err != nil {
			return true
		}
		current = got
		retained = got.Status == thread.StatusCompleted && got.Error != ""
		return retained
	}, 5*time.Second, 5*time.Millisecond)
	return current, retained
}

func requireCleanupPreserved(t *testing.T, mgr *thread.Manager, repo string, st thread.Thread) thread.Thread {
	t.Helper()
	got, retained := waitForThreadCleanup(t, mgr, st.ID)
	require.True(t, retained)
	require.DirExists(t, st.WorktreePath)
	require.NotEmpty(t, strings.TrimSpace(runGit(t, repo, "branch", "--list", st.Branch)))
	return got
}

func TestSafeCleanup_RemovesCleanBranchWithoutUniqueCommits(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "clean", Goal: "finish"})
	require.NoError(t, err)

	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.Eventually(t, func() bool {
		_, getErr := mgr.Get(context.Background(), st.ID)
		return getErr != nil
	}, 5*time.Second, 5*time.Millisecond)
	require.NoDirExists(t, st.WorktreePath)
	require.Empty(t, strings.TrimSpace(runGit(t, repo, "branch", "--list", st.Branch)))
}

func TestSafeCleanup_PreservesUniqueCommit(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "committed", Goal: "finish"})
	require.NoError(t, err)
	writeFile(t, st.WorktreePath, "result.txt", "kept\n")
	runGit(t, st.WorktreePath, "add", "result.txt")
	runGit(t, st.WorktreePath, "commit", "-m", "retain result")

	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	got := requireCleanupPreserved(t, mgr, repo, st)
	require.Contains(t, got.Error, "unique commits")
}

func TestSafeCleanup_PreservesTrackedAndUntrackedChanges(t *testing.T) {
	for _, testCase := range []struct {
		name string
		edit func(*testing.T, thread.Thread)
	}{
		{"tracked", func(t *testing.T, st thread.Thread) { writeFile(t, st.WorktreePath, "README.md", "changed\n") }},
		{"untracked", func(t *testing.T, st thread.Thread) { writeFile(t, st.WorktreePath, "new.txt", "changed\n") }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repo := initRepo(t)
			mgr, spawner := newTestManager(t, repo)
			st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: testCase.name, Goal: "finish"})
			require.NoError(t, err)
			testCase.edit(t, st)
			publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
			got := requireCleanupPreserved(t, mgr, repo, st)
			require.Contains(t, got.Error, "uncommitted changes")
		})
	}
}

func TestSafeCleanup_PreservesConflictAndMissingBase(t *testing.T) {
	for _, name := range []string{"conflict", "missing-base"} {
		t.Run(name, func(t *testing.T) {
			repo := initRepo(t)
			mgr, spawner := newTestManager(t, repo)
			st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: name, Goal: "finish"})
			require.NoError(t, err)
			if name == "conflict" {
				writeFile(t, st.WorktreePath, "README.md", "thread\n")
				runGit(t, st.WorktreePath, "add", "README.md")
				runGit(t, st.WorktreePath, "commit", "-m", "thread change")
				writeFile(t, repo, "README.md", "base\n")
				runGit(t, repo, "add", "README.md")
				runGit(t, repo, "commit", "-m", "base change")
				cmd := exec.CommandContext(t.Context(), "git", "merge", "main")
				cmd.Dir = st.WorktreePath
				require.Error(t, cmd.Run())
			} else {
				runGit(t, repo, "checkout", "--detach")
				runGit(t, repo, "branch", "-D", "main")
			}
			publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
			got := requireCleanupPreserved(t, mgr, repo, st)
			if name == "missing-base" {
				require.Contains(t, got.Error, "could not be verified")
			}
		})
	}
}

func TestSafeCleanup_CancellationPreservesResources(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "cancelled", Goal: "finish"})
	require.NoError(t, err)
	require.NoError(t, mgr.Cancel(t.Context(), st.ID, "stop"))
	require.DirExists(t, st.WorktreePath)
	_, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
}

func TestSafeCleanup_GitProbeErrorPreservesResources(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "probe-error", Goal: "finish"})
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(st.WorktreePath, ".git")))
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	got := requireCleanupPreserved(t, mgr, repo, st)
	require.Contains(t, got.Error, "could not be verified")
}

func TestSafeCleanup_RemovalFailuresReportActualResources(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		configure  func(*thread.ManagerOptions, *flakyStore)
		wantRow    bool
		wantTree   bool
		wantBranch bool
	}{
		{"worktree error before mutation", func(opts *thread.ManagerOptions, _ *flakyStore) {
			opts.WorktreeRemove = func(context.Context, string, string, bool) error { return errors.New("blocked") }
		}, true, true, true},
		{"worktree mutation then error", func(opts *thread.ManagerOptions, _ *flakyStore) {
			opts.WorktreeRemove = func(ctx context.Context, repo, path string, force bool) error {
				if err := git.WorktreeRemove(ctx, repo, path, force); err != nil {
					return err
				}
				return errors.New("post-remove failure")
			}
		}, true, false, true},
		{"branch mutation then error", func(opts *thread.ManagerOptions, _ *flakyStore) {
			opts.DeleteBranch = func(ctx context.Context, repo, branch string, force bool) error {
				if err := git.DeleteBranch(ctx, repo, branch, force); err != nil {
					return err
				}
				return errors.New("post-delete failure")
			}
		}, true, false, false},
		{"record error before mutation", func(_ *thread.ManagerOptions, store *flakyStore) {
			store.failDelete = true
		}, true, false, false},
		{"record mutation then error", func(_ *thread.ManagerOptions, store *flakyStore) {
			store.deleteThenError = true
		}, false, false, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repo := initRepo(t)
			store := &flakyStore{Store: thread.NewStoreForTest(t)}
			spawner := newFakeSpawner(t)
			opts := thread.ManagerOptions{Store: store, Spawner: spawner, RepoRoot: repo, WorktreeDir: t.TempDir()}
			testCase.configure(&opts, store)
			mgr := thread.NewManager(opts)
			shutdownManagerOnCleanup(t, mgr)
			st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: strings.ReplaceAll(testCase.name, " ", "-"), Goal: "finish"})
			require.NoError(t, err)
			publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)

			require.Eventually(t, func() bool {
				got, getErr := mgr.Get(context.Background(), st.ID)
				if !testCase.wantRow {
					return getErr != nil
				}
				return getErr == nil && got.Status == thread.StatusCompleted && got.Error != ""
			}, 5*time.Second, 5*time.Millisecond)
			require.Eventually(t, func() bool {
				return pathExistsForTest(st.WorktreePath) == testCase.wantTree &&
					(strings.TrimSpace(runGit(t, repo, "branch", "--list", st.Branch)) != "") == testCase.wantBranch
			}, 5*time.Second, 5*time.Millisecond)
			if testCase.wantRow {
				got, getErr := mgr.Get(t.Context(), st.ID)
				require.NoError(t, getErr)
				require.Equal(t, thread.StatusCompleted, got.Status)
				require.Contains(t, got.Error, "could not be verified")
			}
		})
	}
}
