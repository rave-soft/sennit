package threadspawn

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

func newAttachTestApp(t *testing.T, path string) *app.App {
	t.Helper()
	t.Setenv("BRAID_GLOBAL_CONFIG", t.TempDir())
	boot, err := app.Bootstrap(t.Context(), path, app.BootstrapOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		boot.App.Shutdown()
		db.ResetPool()
	})
	return boot.App
}

func TestAttachOnlyAtRepositoryRoot(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)

	Attach(t.Context(), a, repo, newAttachTestSpawner(t))
	require.IsType(t, (*thread.Manager)(nil), a.ThreadManager())
	require.NotNil(t, a.Threads)

	subdir := filepath.Join(repo, "subdir")
	require.NoError(t, os.Mkdir(subdir, 0o755))
	nested := newAttachTestApp(t, subdir)
	Attach(t.Context(), nested, subdir, newAttachTestSpawner(t))
	require.Nil(t, nested.ThreadManager())
	require.Nil(t, nested.Threads)
}

func TestAttachDoesNotPublishWhenConnectFails(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	deps := testAttachDeps()
	deps.connect = func(context.Context, string) (*sql.DB, error) { return nil, errors.New("connect") }

	AttachWithDeps(t.Context(), a, repo, newAttachTestSpawner(t), deps)

	require.Nil(t, a.ThreadManager())
	require.Nil(t, a.Threads)
}

func TestAttachRecoversBestEffortAndPublishes(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	deps := testAttachDeps()
	deps.recover = func(*thread.Manager, context.Context) error { return errors.New("recover") }

	AttachWithDeps(t.Context(), a, repo, newAttachTestSpawner(t), deps)

	require.IsType(t, (*thread.Manager)(nil), a.ThreadManager())
	require.NotNil(t, a.Threads)
}

func TestAttachShutdownHookFailureReleasesDatabaseWithoutPublishing(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	deps := testAttachDeps()
	var releases int
	deps.addShutdownHook = func(*app.App, func(context.Context) error) error { return errors.New("hook") }
	deps.release = func(dir string) error {
		releases++
		return db.Release(dir)
	}

	AttachWithDeps(t.Context(), a, repo, newAttachTestSpawner(t), deps)

	require.Equal(t, 1, releases)
	require.Nil(t, a.ThreadManager())
	require.Nil(t, a.Threads)
}

func TestAttachCriticalCleanupFailureShutsDownThenReleasesWithoutPublishing(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	deps := testAttachDeps()
	var calls []string
	deps.addCriticalCleanup = func(*app.App, func(context.Context) error) error { return errors.New("cleanup") }
	deps.shutdown = func(mgr *thread.Manager, ctx context.Context) error {
		calls = append(calls, "shutdown")
		return mgr.Shutdown(ctx)
	}
	deps.release = func(dir string) error {
		calls = append(calls, "release")
		return db.Release(dir)
	}

	AttachWithDeps(t.Context(), a, repo, newAttachTestSpawner(t), deps)

	require.Equal(t, []string{"shutdown", "release"}, calls)
	require.Nil(t, a.ThreadManager())
	require.Nil(t, a.Threads)
}

func testAttachDeps() attachDeps { return ProductionAttachDeps() }

func TestAttachCleanupShutsDownManagerBeforeReleasingDatabase(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	deps := testAttachDeps()
	var shutdownHook, criticalCleanup func(context.Context) error
	var calls []string
	deps.addShutdownHook = func(_ *app.App, fn func(context.Context) error) error {
		shutdownHook = fn
		return nil
	}
	deps.addCriticalCleanup = func(_ *app.App, fn func(context.Context) error) error {
		criticalCleanup = fn
		return nil
	}
	deps.shutdown = func(mgr *thread.Manager, ctx context.Context) error {
		calls = append(calls, "shutdown")
		return mgr.Shutdown(ctx)
	}
	deps.release = func(dir string) error {
		calls = append(calls, "release")
		return db.Release(dir)
	}

	AttachWithDeps(t.Context(), a, repo, newAttachTestSpawner(t), deps)

	require.IsType(t, (*thread.Manager)(nil), a.ThreadManager())
	require.NotNil(t, a.Threads)
	require.NotNil(t, shutdownHook)
	require.NotNil(t, criticalCleanup)
	require.NoError(t, shutdownHook(t.Context()))
	require.NoError(t, criticalCleanup(t.Context()))
	require.Equal(t, []string{"shutdown", "release"}, calls)

	conn, err := db.Connect(context.Background(), config.GlobalDBDir())
	require.NoError(t, err)
	require.NotNil(t, conn)
	require.NoError(t, db.Release(config.GlobalDBDir()))
}

func TestAttachPublishesEventsAndReleasesStoreOnShutdown(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	ch := a.Events(t.Context())

	Attach(t.Context(), a, repo, newAttachTestSpawner(t))
	mgr, ok := a.ThreadManager().(*thread.Manager)
	require.True(t, ok)

	// Let ForwardEvents subscribe before publishing, as production event tests
	// do for post-construction sources.
	time.Sleep(10 * time.Millisecond)
	mgr.PublishForTest(thread.EventStatusChanged, thread.Thread{Delegation: thread.Delegation{ID: "thread-1", Name: "one"}})
	timeout := time.After(5 * time.Second)
waitForThreadEvent:
	for {
		select {
		case event := <-ch:
			forwarded, ok := event.Payload.(pubsub.Event[thread.Event])
			if !ok {
				continue
			}
			require.Equal(t, "thread-1", forwarded.Payload.Thread.ID)
			break waitForThreadEvent
		case <-timeout:
			t.Fatal("timed out waiting for forwarded thread event")
		}
	}

	a.Shutdown()
	// Shutdown has run the manager hook before the critical DB cleanup. A new
	// pooled connection proves the attached reference was released rather than
	// leaking after its owner stopped.
	conn, err := db.Connect(context.Background(), config.GlobalDBDir())
	require.NoError(t, err)
	require.NotNil(t, conn)
	require.NoError(t, db.Release(config.GlobalDBDir()))
}

// TestAttach_TaskManagerReachableAndSharesRecoverySweep proves the task
// manager is wired up by Attach the way NewTaskManager requires: reachable
// off the app, its Spawner wrapping the attached App itself rather than a
// new one, and sharing the thread manager's lifecycle strongly enough
// that a task and a thread created through them both get reconciled by
// one Manager.Recover call.
func TestAttach_TaskManagerReachableAndSharesRecoverySweep(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	// Swap in fakes the same way fakeSpawner does for a thread's own
	// isolated App, so a task's dispatch (which runs inside a itself,
	// per ParentAppSpawner) is deterministic instead of hitting a real
	// LLM/session store.
	a.SetSessionsForTest(&attachFakeSessions{})
	a.AgentCoordinator = &attachFakeCoordinator{}

	Attach(t.Context(), a, repo, newAttachTestSpawner(t))

	mgr, ok := a.ThreadManager().(*thread.Manager)
	require.True(t, ok, "thread manager should be reachable off the app after attach")
	tasks, ok := a.TaskManager().(*thread.TaskManager)
	require.True(t, ok, "task manager should be reachable off the app after attach")

	taskSt, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)

	// The task's runtime wraps the attached App itself, not a new one —
	// Manager.Handle is kind-agnostic, so it reaches a task's runtime too.
	h := mgr.Handle(taskSt.ID)
	require.NotNil(t, h)
	// A task's runtime is produced by Attach's NewParentAppSpawner, so its
	// handle is the production *parentHandle wrapping the attached App —
	// not the thread-spawner's attachTestHandle.
	adh, ok := h.(*parentHandle)
	require.True(t, ok)
	parentWorkspace, ok := adh.Workspace().(*AppWorkspaceAdapter)
	require.True(t, ok)
	require.Same(t, a, parentWorkspace.App)

	threadSt, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "sibling-thread", Goal: "go", MergePolicy: thread.MergeManual})
	require.NoError(t, err)

	// Leave both dispatched runs in flight (no RunComplete published for
	// either) and reconcile through a single recovery sweep. If
	// TaskManager had its own lifecycle instead of sharing mgr's, this
	// sweep would only ever see the thread.
	require.NoError(t, mgr.Recover(t.Context()))

	gotTask, err := mgr.Get(t.Context(), taskSt.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusInterrupted, gotTask.Status, "the task must be reachable through the same recovery sweep as the thread")

	gotThread, err := mgr.Get(t.Context(), threadSt.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusInterrupted, gotThread.Status)
}

// TestAttach_ShutdownJoinsBothKinds proves the shutdown hook Attach
// registers (mgr.Shutdown, wired via addShutdownHook) already joins a
// task's in-flight run the same way it does a thread's, without any
// teardown code added for the task manager specifically: both share
// mgr's lifecycle, so mgr.Shutdown's single controls-map walk reaches
// both.
func TestAttach_ShutdownJoinsBothKinds(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)
	a.SetSessionsForTest(&attachFakeSessions{})
	a.AgentCoordinator = &attachFakeCoordinator{}

	threadSpawner := newAttachTestSpawner(t)
	Attach(t.Context(), a, repo, threadSpawner)

	mgr, ok := a.ThreadManager().(*thread.Manager)
	require.True(t, ok)
	tasks, ok := a.TaskManager().(*thread.TaskManager)
	require.True(t, ok)

	taskSt, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	threadSt, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "sibling-thread", Goal: "go", MergePolicy: thread.MergeManual})
	require.NoError(t, err)

	taskCoord := a.AgentCoordinator.(*attachFakeCoordinator)
	require.Eventually(t, func() bool { return taskCoord.runCount() == 1 }, time.Second, time.Millisecond)
	threadCoord := threadSpawner.coordFor(threadSt.WorktreePath)
	require.Eventually(t, func() bool { return threadCoord.runCount() == 1 }, time.Second, time.Millisecond)
	// Deliberately no RunComplete published for either: both runs are
	// left in flight, the way TestTaskManager_ShutdownJoinsInFlightRun
	// and the thread-only shutdown tests in manager_test.go leave theirs.

	// Call mgr.Shutdown directly rather than a.Shutdown: this is exactly
	// the function Attach registered as the app's shutdown hook (see
	// deps.shutdown), and calling it directly — rather than through the
	// full app shutdown, which also releases the underlying DB
	// connection — leaves the store queryable afterward to assert on.
	require.NoError(t, mgr.Shutdown(t.Context()))

	gotTask, err := mgr.Get(t.Context(), taskSt.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusInterrupted, gotTask.Status)

	gotThread, err := mgr.Get(t.Context(), threadSt.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusInterrupted, gotThread.Status)
}
