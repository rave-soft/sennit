package thread_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

type failingReleaseSpawner struct {
	thread.Spawner
	fail bool
	mu   sync.Mutex
}

func (s *failingReleaseSpawner) setFailure(fail bool) {
	s.mu.Lock()
	s.fail = fail
	s.mu.Unlock()
}

func (s *failingReleaseSpawner) Release(ctx context.Context, id string) error {
	s.mu.Lock()
	fail := s.fail
	s.mu.Unlock()
	if fail {
		return errors.New("runtime still owns worktree")
	}
	return s.Spawner.Release(ctx, id)
}

type partialHandleSpawner struct {
	thread.Spawner
	partial bool
}

func (s *partialHandleSpawner) Spawn(ctx context.Context, request thread.SpawnRequest) (thread.Handle, error) {
	handle, err := s.Spawner.Spawn(ctx, request)
	if err == nil && s.partial {
		return handle, errors.New("partial bootstrap failed")
	}
	return handle, err
}

func TestShutdownRetainsFailedOwnerAndAllowsReleaseRetry(t *testing.T) {
	repo := initRepo(t)
	spawner := &failingReleaseSpawner{Spawner: newFakeSpawner(t), fail: true}
	manager := thread.NewManager(thread.ManagerOptions{Store: thread.NewStoreForTest(t), Spawner: spawner, RepoRoot: repo})
	t.Cleanup(func() {
		spawner.setFailure(false)
		require.NoError(t, manager.Shutdown(context.Background()))
	})
	row, err := manager.Create(t.Context(), thread.CreateArgs{Name: "shutdown-release"})
	require.NoError(t, err)
	require.ErrorContains(t, manager.Shutdown(t.Context()), "runtime still owns worktree")
	require.NotNil(t, manager.Handle(row.ID))
	_, err = os.Stat(row.WorktreePath)
	require.NoError(t, err)
	spawner.setFailure(false)
	require.NoError(t, manager.Shutdown(t.Context()))
	require.Nil(t, manager.Handle(row.ID))
	_, err = os.Stat(row.WorktreePath)
	require.NoError(t, err)
}

func TestPartialResumeHandleRemainsOwnedUntilReleased(t *testing.T) {
	for _, activate := range []bool{false, true} {
		t.Run(map[bool]string{false: "send", true: "activate"}[activate], func(t *testing.T) {
			repo := initRepo(t)
			partial := &partialHandleSpawner{Spawner: newFakeSpawner(t)}
			spawner := &failingReleaseSpawner{Spawner: partial}
			manager := thread.NewManager(thread.ManagerOptions{Store: thread.NewStoreForTest(t), Spawner: spawner, RepoRoot: repo})
			shutdownManagerOnCleanup(t, manager)
			t.Cleanup(func() { spawner.setFailure(false) })
			row, err := manager.Create(t.Context(), thread.CreateArgs{Name: "partial-resume"})
			require.NoError(t, err)
			require.NoError(t, manager.Cancel(t.Context(), row.ID, "stop"))
			partial.partial = true
			spawner.setFailure(true)
			if activate {
				_, err = manager.Activate(t.Context(), row.ID)
			} else {
				_, err = manager.Send(t.Context(), row.ID, "resume")
			}
			require.ErrorContains(t, err, "partial bootstrap failed")
			require.NotNil(t, manager.Handle(row.ID))
			require.ErrorContains(t, manager.Remove(t.Context(), row.ID, true, true), "runtime still owns worktree")
			_, err = os.Stat(row.WorktreePath)
			require.NoError(t, err)
			spawner.setFailure(false)
			require.NoError(t, manager.Remove(t.Context(), row.ID, true, true))
		})
	}
}

func TestResumeRollbackRetainsFailedOwner(t *testing.T) {
	for _, activate := range []bool{false, true} {
		t.Run(map[bool]string{false: "send", true: "activate"}[activate], func(t *testing.T) {
			repo := initRepo(t)
			spawner := &failingReleaseSpawner{Spawner: newFakeSpawner(t)}
			store := &flakyStore{Store: thread.NewStoreForTest(t), failSetStatus: map[thread.Status]bool{}}
			manager := thread.NewManager(thread.ManagerOptions{Store: store, Spawner: spawner, RepoRoot: repo})
			shutdownManagerOnCleanup(t, manager)
			t.Cleanup(func() { spawner.setFailure(false) })
			row, err := manager.Create(t.Context(), thread.CreateArgs{Name: "resume-rollback"})
			require.NoError(t, err)
			require.NoError(t, manager.Cancel(t.Context(), row.ID, "stop"))
			spawner.setFailure(true)
			if activate {
				store.failSetStatus[thread.StatusIdle] = true
				_, err = manager.Activate(t.Context(), row.ID)
			} else {
				store.failSetStatus[thread.StatusRunning] = true
				_, err = manager.Send(t.Context(), row.ID, "resume")
			}
			require.ErrorContains(t, err, "forced SetStatus failure")
			require.NotNil(t, manager.Handle(row.ID))
			require.ErrorContains(t, manager.Remove(t.Context(), row.ID, true, true), "runtime still owns worktree")
			_, err = os.Stat(row.WorktreePath)
			require.NoError(t, err)
			spawner.setFailure(false)
			require.NoError(t, manager.Remove(t.Context(), row.ID, true, true))
		})
	}
}

func TestCreateRollbackPreservesWorktreeOnReleaseFailure(t *testing.T) {
	repo := initRepo(t)
	underlying := newFakeSpawner(t)
	underlying.sessionsErr = errors.New("session preparation failed")
	spawner := &failingReleaseSpawner{Spawner: underlying, fail: true}
	store := thread.NewStoreForTest(t)
	manager := thread.NewManager(thread.ManagerOptions{Store: store, Spawner: spawner, RepoRoot: repo})
	shutdownManagerOnCleanup(t, manager)
	t.Cleanup(func() { spawner.setFailure(false) })
	_, err := manager.Create(t.Context(), thread.CreateArgs{Name: "rollback-release"})
	require.ErrorContains(t, err, "session preparation failed")
	rows, err := store.ListAll(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	_, err = os.Stat(rows[0].WorktreePath)
	require.NoError(t, err)
	require.NotNil(t, manager.Handle(rows[0].ID))
	spawner.setFailure(false)
	require.NoError(t, manager.Remove(t.Context(), rows[0].ID, true, true))
}

func TestRemovePreservesWorktreeUntilRuntimeReleaseSucceeds(t *testing.T) {
	repo := initRepo(t)
	spawner := &failingReleaseSpawner{Spawner: newFakeSpawner(t), fail: true}
	manager := thread.NewManager(thread.ManagerOptions{
		Store: thread.NewStoreForTest(t), Spawner: spawner, RepoRoot: repo,
	})
	shutdownManagerOnCleanup(t, manager)
	t.Cleanup(func() { spawner.setFailure(false) })
	row, err := manager.Create(t.Context(), thread.CreateArgs{Name: "release-failure"})
	require.NoError(t, err)
	require.ErrorContains(t, manager.Remove(t.Context(), row.ID, true, true), "runtime still owns worktree")
	_, err = os.Stat(row.WorktreePath)
	require.NoError(t, err)
	_, err = manager.Get(t.Context(), row.ID)
	require.NoError(t, err)
	_, err = manager.Send(t.Context(), row.ID, "must not reuse failed runtime")
	require.ErrorContains(t, err, "runtime still owns worktree")
	_, err = manager.Activate(t.Context(), row.ID)
	require.ErrorContains(t, err, "runtime still owns worktree")
	spawner.setFailure(false)
	require.NoError(t, manager.Remove(t.Context(), row.ID, true, true))
	_, err = os.Stat(row.WorktreePath)
	require.True(t, os.IsNotExist(err))
}
