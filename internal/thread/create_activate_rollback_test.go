package thread_test

// This file proves Manager.Create and Manager.Activate unwind exactly the
// steps that succeeded before a failure, in reverse, and never touch a
// step that never ran — the invariant the [unwinder] mechanism (unexported,
// in rollback.go) exists to hold everywhere those two methods build up a
// git worktree and a spawned workspace before either resting somewhere
// safe or reporting a failure.
//
// Every case below fails one specific step and checks, from the outside
// (worktree presence on disk, spawner release counts), that only the
// steps before it were undone.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

// slug turns a Go test name (which may contain underscores) into a valid
// thread name (lowercase alphanumeric, hyphen-separated — see
// thread.CreateArgs.Name's validation).
func slug(name string) string {
	return strings.ReplaceAll(name, "_", "-")
}

// flakyStore wraps a real Store and lets a test make one specific write
// fail on demand, without reimplementing the whole persistence contract —
// the only way to reach Manager.Create/Activate's later rollback steps,
// which a real store's own validation can't be made to fail any other way.
type flakyStore struct {
	thread.Store
	failSetSession bool
	failSetStatus  map[thread.Status]bool
}

func (s *flakyStore) SetSession(ctx context.Context, id, sessionID string) (thread.Thread, error) {
	if s.failSetSession {
		return thread.Thread{}, errors.New("flakyStore: forced SetSession failure")
	}
	return s.Store.SetSession(ctx, id, sessionID)
}

func (s *flakyStore) SetStatus(ctx context.Context, id string, params thread.SetStatusParams) (thread.Thread, error) {
	if s.failSetStatus[params.Status] {
		return thread.Thread{}, errors.New("flakyStore: forced SetStatus failure")
	}
	return s.Store.SetStatus(ctx, id, params)
}

// worktreeExists reports whether path is present on disk, the signal these
// tests use for "was the worktree rolled back".
func worktreeExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	require.True(t, os.IsNotExist(err), "unexpected stat error: %v", err)
	return false
}

// TestManager_CreateRollsBackOnlyWhatSucceeded is the table-driven proof for
// Create's rollback: each case fails one step and asserts that every step
// before it was undone (worktree removed, spawned workspace released) and
// that nothing past the failing step ever ran (no session recorded, no
// dispatch).
func TestManager_CreateRollsBackOnlyWhatSucceeded(t *testing.T) {
	type setup struct {
		// args customizes CreateArgs for this case (name is unique per
		// subtest already).
		args func(a *thread.CreateArgs)
		// spawner configures the spawner to fail (or observe) at this
		// case's step.
		spawner func(s *fakeSpawner, cancel context.CancelFunc)
		// store wraps the manager's store for this case's step.
		store func(real thread.Store) thread.Store
	}

	cases := []struct {
		name string
		setup
		// wantWorktree/wantReleased describe the state rollback should
		// leave behind: the worktree gone (or not, for the step-1 case
		// where it was never created) and the spawned handle released (or
		// never spawned at all).
		wantWorktree bool
		wantSpawns   int
		wantReleases int
	}{
		{
			// Step 1: git.WorktreeAdd itself fails (bad base branch).
			// Nothing has succeeded yet, so there is nothing to unwind.
			name: "worktree_add_fails",
			setup: setup{
				args: func(a *thread.CreateArgs) { a.BaseBranch = "does-not-exist" },
			},
			wantWorktree: false,
			wantSpawns:   0,
			wantReleases: 0,
		},
		{
			// Step 2: spawning the workspace fails. Only the worktree
			// (step 1) had succeeded, so only it is undone.
			name: "spawn_fails",
			setup: setup{
				spawner: func(s *fakeSpawner, _ context.CancelFunc) { s.spawnErr = errors.New("spawn boom") },
			},
			wantWorktree: false,
			wantSpawns:   1,
			wantReleases: 0, // Spawn itself failed: there is no handle to release.
		},
		{
			// Step 3: the manager's context is canceled in the window
			// between a successful spawn and Create's own ctx.Err() check.
			// Both earlier steps (worktree, spawn) are undone.
			name: "context_canceled_after_spawn",
			setup: setup{
				spawner: func(s *fakeSpawner, cancel context.CancelFunc) {
					s.afterSpawn = func(string) { cancel() }
				},
			},
			wantWorktree: false,
			wantSpawns:   1,
			wantReleases: 1,
		},
		{
			// Step 4: session creation fails. Worktree and spawn (steps 1
			// and 2) are undone.
			name: "session_create_fails",
			setup: setup{
				spawner: func(s *fakeSpawner, _ context.CancelFunc) { s.sessionsErr = errors.New("session boom") },
			},
			wantWorktree: false,
			wantSpawns:   1,
			wantReleases: 1,
		},
		{
			// Step 5: store.SetSession fails, after a session was already
			// created in the (about-to-be-destroyed) spawned workspace.
			// Worktree and spawn are still undone; the session lived only
			// inside the workspace being torn down, so there is nothing
			// separate to undo for it.
			name: "set_session_fails",
			setup: setup{
				store: func(real thread.Store) thread.Store { return &flakyStore{Store: real, failSetSession: true} },
			},
			wantWorktree: false,
			wantSpawns:   1,
			wantReleases: 1,
		},
		{
			// Step 6: the idle-path setStatus (empty Goal) fails, after
			// every earlier step succeeded.
			name: "idle_set_status_fails",
			setup: setup{
				args: func(a *thread.CreateArgs) { a.Goal = "" },
				store: func(real thread.Store) thread.Store {
					return &flakyStore{Store: real, failSetStatus: map[thread.Status]bool{thread.StatusIdle: true}}
				},
			},
			wantWorktree: false,
			wantSpawns:   1,
			wantReleases: 1,
		},
		{
			// Step 7: the running-path setStatus (non-empty Goal) fails.
			name: "running_set_status_fails",
			setup: setup{
				store: func(real thread.Store) thread.Store {
					return &flakyStore{Store: real, failSetStatus: map[thread.Status]bool{thread.StatusRunning: true}}
				},
			},
			wantWorktree: false,
			wantSpawns:   1,
			wantReleases: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			spawner := newFakeSpawner(t)
			if tc.spawner != nil {
				tc.spawner(spawner, cancel)
			}
			store := thread.NewStoreForTest(t)
			if tc.store != nil {
				store = tc.store(store)
			}
			mgr := thread.NewManager(thread.ManagerOptions{
				Store:       store,
				Spawner:     spawner,
				RepoRoot:    repo,
				WorktreeDir: t.TempDir(),
				Context:     ctx,
			})
			shutdownManagerOnCleanup(t, mgr)

			args := thread.CreateArgs{Name: "rb-" + slug(tc.name), Goal: "go", MergePolicy: thread.MergeManual}
			if tc.args != nil {
				tc.args(&args)
			}
			worktreePath := filepath.Join(mgr.WorktreeDirForTest(), args.Name)

			_, err := mgr.Create(context.Background(), args)
			require.Error(t, err)

			require.Equal(t, tc.wantWorktree, worktreeExists(t, worktreePath))
			require.Equal(t, tc.wantSpawns, spawner.spawns())
			require.Equal(t, tc.wantReleases, spawner.releases(worktreePath))
		})
	}
}

// TestManager_CreateRollbackOrder_ReleasesBeforeRemovingWorktree pins the
// unwinder's ordering invariant that end-state assertions alone can't see:
// a spawned workspace is released before the worktree it was spawned into
// is removed, because the reverse order (removing the worktree while the
// spawned workspace's own files still live under it) is exactly the class
// of failure `sennit gc`'s orphaned-worktree report exists to catch — see
// unwinder's doc comment. Steps 1 (worktree) and 2 (spawn) both succeed
// here; step 4 (session creation) is what fails and triggers the unwind.
func TestManager_CreateRollbackOrder_ReleasesBeforeRemovingWorktree(t *testing.T) {
	repo := initRepo(t)
	spawner := newFakeSpawner(t)
	spawner.sessionsErr = errors.New("session boom")
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       thread.NewStoreForTest(t),
		Spawner:     spawner,
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	shutdownManagerOnCleanup(t, mgr)

	args := thread.CreateArgs{Name: "rollback-order", Goal: "go", MergePolicy: thread.MergeManual}
	worktreePath := filepath.Join(mgr.WorktreeDirForTest(), args.Name)

	_, err := mgr.Create(context.Background(), args)
	require.Error(t, err)

	require.Equal(t, 1, spawner.releases(worktreePath))
	require.False(t, worktreeExists(t, worktreePath), "worktree should have been removed by the unwind")
	require.True(t, spawner.releaseSawWorktreeAt(worktreePath),
		"Release ran while the worktree still existed on disk, i.e. before WorktreeRemove — the required reverse order")
}

// TestManager_ActivateRollsBackOnlyWhatSucceeded is Activate's counterpart
// to TestManager_CreateRollsBackOnlyWhatSucceeded: it only has a spawned
// workspace to build (the worktree already exists on disk from an earlier
// Create), so there are three steps rather than seven.
func TestManager_ActivateRollsBackOnlyWhatSucceeded(t *testing.T) {
	cases := []struct {
		name         string
		spawner      func(s *fakeSpawner, cancel context.CancelFunc)
		store        func(real thread.Store) thread.Store
		wantReleases int
	}{
		{
			// Step 1: spawning fails. Nothing succeeded, nothing to undo.
			name:         "spawn_fails",
			spawner:      func(s *fakeSpawner, _ context.CancelFunc) { s.spawnErr = errors.New("spawn boom") },
			wantReleases: 0,
		},
		{
			// Step 2: the manager's context is canceled between a
			// successful spawn and Activate's own ctx.Err() check.
			name: "context_canceled_after_spawn",
			spawner: func(s *fakeSpawner, cancel context.CancelFunc) {
				s.afterSpawn = func(string) { cancel() }
			},
			wantReleases: 1,
		},
		{
			// Step 3: setStatus fails, after the workspace was
			// successfully respawned.
			name: "set_status_fails",
			store: func(real thread.Store) thread.Store {
				return &flakyStore{Store: real, failSetStatus: map[thread.Status]bool{thread.StatusIdle: true}}
			},
			wantReleases: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t)
			worktreeDir := t.TempDir()
			realStore := thread.NewStoreForTest(t)

			// Build the thread with a plain manager over the same store
			// first, so its worktree exists and its run is finished
			// (Activate refuses a live one). The case under test then
			// reopens it through its own manager/context, sharing that
			// store (wrapped, for the cases that need to fail one call)
			// so the injected failure only fires on the Activate call.
			setupSpawner := newFakeSpawner(t)
			setupMgr := thread.NewManager(thread.ManagerOptions{
				Store:       realStore,
				Spawner:     setupSpawner,
				RepoRoot:    repo,
				WorktreeDir: worktreeDir,
			})
			shutdownManagerOnCleanup(t, setupMgr)
			st, err := setupMgr.Create(context.Background(), thread.CreateArgs{Name: "act-" + slug(tc.name), Goal: "go", MergePolicy: thread.MergeManual})
			require.NoError(t, err)
			publishSuccess(t, setupSpawner.appFor(st.WorktreePath), st.SessionID)
			require.NoError(t, setupMgr.Wait(context.Background(), []string{st.ID}, settleTimeout))

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			spawner := newFakeSpawner(t)
			if tc.spawner != nil {
				tc.spawner(spawner, cancel)
			}
			store := realStore
			if tc.store != nil {
				store = tc.store(store)
			}
			mgr := thread.NewManager(thread.ManagerOptions{
				Store:       store,
				Spawner:     spawner,
				RepoRoot:    repo,
				WorktreeDir: worktreeDir,
				Context:     ctx,
			})
			shutdownManagerOnCleanup(t, mgr)

			_, err = mgr.Activate(context.Background(), st.ID)
			require.Error(t, err)
			require.Equal(t, tc.wantReleases, spawner.releases(st.WorktreePath))
		})
	}
}

// TestManager_CreateFailureMarksTheRightRow proves that every failure path
// in Create reachable after store.Create has run — store.SetSession's own
// failure (the original defect: `st, err = m.store.SetSession(...)`
// clobbered st with a zero Thread before the error was checked, so
// failCreate marked an empty ID and the real row's status never changed),
// the ctx.Err() check after Spawn, and the two setStatus calls further
// down — all leave the thread's own row at StatusFailed with cause recorded
// as its Error, rather than stranded at whatever status Create left it in.
//
// Reverting the failCreate fix (restoring `st, err =
// m.store.SetSession(...)`, or dropping the ctx.Err()/setStatus failCreate
// calls) makes the "set_session_fails" case fail outright (status stays
// StatusPending) and the other three fail on the Error-contains assertion
// (status still flips via later cleanup? no -- it stays whatever Create
// left it at, never StatusFailed, and Error stays empty).
func TestManager_CreateFailureMarksTheRightRow(t *testing.T) {
	cases := []struct {
		name       string
		args       func(a *thread.CreateArgs)
		spawner    func(s *fakeSpawner, cancel context.CancelFunc)
		store      func(real thread.Store) thread.Store
		wantErrSub string
	}{
		{
			name: "set_session_fails",
			store: func(real thread.Store) thread.Store {
				return &flakyStore{Store: real, failSetSession: true}
			},
			wantErrSub: "forced SetSession failure",
		},
		{
			name: "context_canceled_after_spawn",
			spawner: func(s *fakeSpawner, cancel context.CancelFunc) {
				s.afterSpawn = func(string) { cancel() }
			},
			wantErrSub: context.Canceled.Error(),
		},
		{
			name: "idle_set_status_fails",
			args: func(a *thread.CreateArgs) { a.Goal = "" },
			store: func(real thread.Store) thread.Store {
				return &flakyStore{Store: real, failSetStatus: map[thread.Status]bool{thread.StatusIdle: true}}
			},
			wantErrSub: "forced SetStatus failure",
		},
		{
			name: "running_set_status_fails",
			store: func(real thread.Store) thread.Store {
				return &flakyStore{Store: real, failSetStatus: map[thread.Status]bool{thread.StatusRunning: true}}
			},
			wantErrSub: "forced SetStatus failure",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			spawner := newFakeSpawner(t)
			if tc.spawner != nil {
				tc.spawner(spawner, cancel)
			}
			store := thread.NewStoreForTest(t)
			if tc.store != nil {
				store = tc.store(store)
			}
			mgr := thread.NewManager(thread.ManagerOptions{
				Store:       store,
				Spawner:     spawner,
				RepoRoot:    repo,
				WorktreeDir: t.TempDir(),
				Context:     ctx,
			})
			shutdownManagerOnCleanup(t, mgr)

			args := thread.CreateArgs{Name: "fc-" + slug(tc.name), Goal: "go", MergePolicy: thread.MergeManual}
			if tc.args != nil {
				tc.args(&args)
			}

			_, err := mgr.Create(context.Background(), args)
			require.Error(t, err)

			// Look the row up by name, exactly as Get's own resolve does:
			// on failure Create never hands the ID back, and this is the
			// only handle a caller has left.
			got, err := mgr.Get(context.Background(), args.Name)
			require.NoError(t, err, "the row must still exist under its name")
			require.Equal(t, thread.StatusFailed, got.Status, "row must be marked failed rather than left at its prior status")
			require.Contains(t, got.Error, tc.wantErrSub)
		})
	}
}
