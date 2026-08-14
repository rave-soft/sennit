package thread

import (
	"context"
	"testing"
	"time"

	"github.com/rave-soft/braid/internal/app"
	"github.com/stretchr/testify/require"
)

// newTestParentApp builds a minimal App standing in for a workspace's own
// running App: the thing a task shares instead of spawning its own,
// wired with the same fakeSessions/fakeCoordinator doubles fakeSpawner
// uses for a thread's isolated App, so a task's run can be driven and
// observed the same way.
func newTestParentApp(t *testing.T) *app.App {
	t.Helper()
	a := app.NewForTest(context.Background())
	t.Cleanup(a.ShutdownForTest)
	a.Sessions = &fakeSessions{}
	a.AgentCoordinator = &fakeCoordinator{}
	return a
}

// newTestTaskManager wires a TaskManager sharing a Manager's lifecycle and
// base context, over a fresh parent App, the way [NewTaskManager]
// requires. The Manager itself never creates a thread in these tests; it
// exists only as the shared lc/ctx source and as the thread-facing entry
// point (Handle, Shutdown) that also reaches task entries, since both
// kinds register in the one lifecycle both share.
func newTestTaskManager(t *testing.T, store Store) (*Manager, *TaskManager, *app.App) {
	t.Helper()
	mgr := NewManager(ManagerOptions{
		Store:    store,
		Spawner:  newFakeSpawner(t),
		RepoRoot: t.TempDir(),
	})
	parentApp := newTestParentApp(t)
	tasks := NewTaskManager(store, NewParentAppSpawner(parentApp), mgr.lc, mgr.ctx)
	return mgr, tasks, parentApp
}

func TestTaskManager_CreateRunsToCompletion(t *testing.T) {
	store := newTestStoreDB(t)
	_, tasks, parentApp := newTestTaskManager(t, store)

	st, err := tasks.Create(t.Context(), TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Equal(t, KindTask, st.Kind)
	require.Empty(t, st.WorktreePath)
	require.Empty(t, st.Branch)
	require.Empty(t, st.BaseBranch)
	require.Equal(t, StatusRunning, st.Status)
	require.NotEmpty(t, st.SessionID)

	// The child session nests under the caller's session, the same
	// relationship a thread's child session gets from Manager.Create.
	sessions := parentApp.Sessions.(*fakeSessions)
	sessions.mu.Lock()
	created := sessions.createdSession
	sessions.mu.Unlock()
	require.Equal(t, st.SessionID, created.ID)
	require.Equal(t, "parent-sess", created.ParentSessionID)

	coord := parentApp.AgentCoordinator.(*fakeCoordinator)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, time.Millisecond)
	publishSuccess(t, parentApp, st.SessionID)

	require.Eventually(t, func() bool {
		got, err := store.Get(t.Context(), st.ID)
		return err == nil && got.Status == StatusCompleted
	}, time.Second, time.Millisecond)
	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, KindTask, got.Kind)
	require.Equal(t, "finished", got.ResultSummary)
}

// TestTaskManager_CreateDoesNotSpawnAppAndReleaseLeavesParentUsable is the
// failure mode that matters most for this delegation kind: a task must
// never get its own isolated App (that is the entire reason it is
// cheaper than a thread), and releasing its runtime once the run
// completes must never tear the shared parent App down.
func TestTaskManager_CreateDoesNotSpawnAppAndReleaseLeavesParentUsable(t *testing.T) {
	store := newTestStoreDB(t)
	mgr, tasks, parentApp := newTestTaskManager(t, store)

	st, err := tasks.Create(t.Context(), TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)

	// No second App was spawned: the task's Handle wraps the exact same
	// App instance the test constructed as the parent.
	h := mgr.Handle(st.ID)
	require.NotNil(t, h)
	require.Same(t, parentApp, h.App())

	coord := parentApp.AgentCoordinator.(*fakeCoordinator)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, time.Millisecond)
	publishSuccess(t, parentApp, st.SessionID)

	// The run completing releases the task's runtime (rt.spawner, a
	// ParentAppSpawner, not the Manager's own thread Spawner).
	require.Eventually(t, func() bool { return mgr.Handle(st.ID) == nil }, time.Second, time.Millisecond)

	// Prove the parent App itself is still alive and usable, not torn
	// down by that release.
	_, err = parentApp.Sessions.Create(t.Context(), "still alive")
	require.NoError(t, err)
}

// TestTaskManager_ShutdownJoinsInFlightRun proves Manager.Shutdown joins a
// task's in-flight run exactly the way it already does a thread's: both
// kinds register in the one shared lifecycle, so the same
// closeAdmission-then-wait sequence covers both without Manager knowing
// tasks exist.
func TestTaskManager_ShutdownJoinsInFlightRun(t *testing.T) {
	store := newTestStoreDB(t)
	mgr, tasks, parentApp := newTestTaskManager(t, store)

	st, err := tasks.Create(t.Context(), TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)

	coord := parentApp.AgentCoordinator.(*fakeCoordinator)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, time.Millisecond)
	// Deliberately no RunComplete published: the run is left in flight,
	// the way an equivalent thread shutdown test leaves a thread's run.

	require.NoError(t, mgr.Shutdown(context.Background()))

	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusInterrupted, got.Status)
	require.Nil(t, mgr.Handle(st.ID))

	// Shutdown released the task's runtime via its own Spawner
	// (ParentAppSpawner.Release), not the Manager's thread Spawner — and,
	// critically, did not shut the parent App down.
	_, err = parentApp.Sessions.Create(t.Context(), "still alive after shutdown")
	require.NoError(t, err)
}
