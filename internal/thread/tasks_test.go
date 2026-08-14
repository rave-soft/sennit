package thread

import (
	"context"
	"testing"
	"time"

	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/permission"
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

// TestTaskManager_CreateAttributesRunToDelegation proves a task's run
// context carries its DelegationRef: a permission request raised by a
// tool the task's run invokes would reach permission.DelegationFromContext
// with the task's identity, not the zero ref a visible turn's tools see.
// The fake coordinator is the same seam other tests use to inspect what a
// dispatched run actually received — no real tool is needed to prove the
// context is threaded through correctly.
func TestTaskManager_CreateAttributesRunToDelegation(t *testing.T) {
	store := newTestStoreDB(t)
	_, tasks, parentApp := newTestTaskManager(t, store)

	st, err := tasks.Create(t.Context(), TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)

	coord := parentApp.AgentCoordinator.(*fakeCoordinator)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, time.Millisecond)

	coord.mu.Lock()
	got := coord.runs[0].delegation
	coord.mu.Unlock()

	require.Equal(t, permission.DelegationRef{ID: st.ID, Name: st.Name, Kind: "task"}, got,
		"a permission request raised by this run must be attributable to the task, not the parent's own turn")
}

// TestTaskManager_ShutdownCancelsOnlyItsOwnSessionNotParentWork is the
// sharp test for the CancelAll-vs-Cancel(sessionID) fix: a task's App is
// its parent's, so tearing a task's runtime down must never cancel other
// work already running in that same App. Before the fix, Shutdown's
// per-runtime teardown called AgentCoordinator.CancelAll() — harmless for
// a thread (its App is its own) but, for a task, indistinguishable from
// cancelling the user's own foreground turn.
func TestTaskManager_ShutdownCancelsOnlyItsOwnSessionNotParentWork(t *testing.T) {
	store := newTestStoreDB(t)
	mgr, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.AgentCoordinator.(*fakeCoordinator)

	// The user's own foreground turn, running in the same App a task
	// shares. This is the work a blunt CancelAll would have caught.
	_, err := coord.Run(t.Context(), "foreground-session", "the user's own prompt")
	require.NoError(t, err)

	st, err := tasks.Create(t.Context(), TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 2 }, time.Second, time.Millisecond)
	// Deliberately no RunComplete published for the task: its run is left
	// in flight, so Shutdown's teardown has something live to cancel.

	require.NoError(t, mgr.Shutdown(context.Background()))

	require.False(t, coord.cancelAllWasCalled(),
		"shutdown must never call CancelAll on a coordinator shared with the parent's own work")
	require.Equal(t, []string{st.SessionID}, coord.canceledSessions(),
		"shutdown must cancel only the task's own session — the foreground session must never appear here")
}

// seedThreadRow inserts a bare kind = KindThread row directly through
// store, bypassing Manager.Create's git machinery entirely: these tests
// only need a thread's id to exist in the shared table, to prove the
// task_* guards reject it.
func seedThreadRow(t *testing.T, store Store) Thread {
	t.Helper()
	st, err := store.Create(t.Context(), CreateParams{
		Name:         "a-thread",
		Goal:         "x",
		BaseBranch:   "main",
		Branch:       "thread/a-thread",
		WorktreePath: t.TempDir(),
		Kind:         KindThread,
	})
	require.NoError(t, err)
	return st
}

func TestTaskManager_ListReturnsOnlyTasks(t *testing.T) {
	store := newTestStoreDB(t)
	_, tasks, _ := newTestTaskManager(t, store)

	task1, err := tasks.Create(t.Context(), TaskCreateArgs{Goal: "a", ParentSessionID: "p1"})
	require.NoError(t, err)
	task2, err := tasks.Create(t.Context(), TaskCreateArgs{Goal: "b", ParentSessionID: "p2"})
	require.NoError(t, err)
	threadRow := seedThreadRow(t, store)

	got, err := tasks.List(t.Context())
	require.NoError(t, err)
	ids := make([]string, len(got))
	for i, st := range got {
		ids[i] = st.ID
	}
	require.ElementsMatch(t, []string{task1.ID, task2.ID}, ids)
	require.NotContains(t, ids, threadRow.ID, "a thread row must never appear in a task listing")
}

func TestTaskManager_GetHappyPath(t *testing.T) {
	store := newTestStoreDB(t)
	_, tasks, _ := newTestTaskManager(t, store)

	created, err := tasks.Create(t.Context(), TaskCreateArgs{Goal: "look into it", ParentSessionID: "p1"})
	require.NoError(t, err)

	got, err := tasks.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)
	require.Equal(t, "look into it", got.Goal)
	require.Equal(t, KindTask, got.Kind)
}

func TestTaskManager_GetRejectsThreadID(t *testing.T) {
	store := newTestStoreDB(t)
	_, tasks, _ := newTestTaskManager(t, store)
	threadRow := seedThreadRow(t, store)

	_, err := tasks.Get(t.Context(), threadRow.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not a task")
}

// TestTaskManager_CancelLeavesTerminalWithReasonAndParentUntouched is the
// sharp test for task_cancel: it must produce a real terminal status
// transition (not merely stop a goroutine), and — since a task's App is
// its parent's — it must reach only the task's own session, never the
// foreground work sharing that same App and coordinator.
func TestTaskManager_CancelLeavesTerminalWithReasonAndParentUntouched(t *testing.T) {
	store := newTestStoreDB(t)
	mgr, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.AgentCoordinator.(*fakeCoordinator)

	_, err := coord.Run(t.Context(), "foreground-session", "the user's own prompt")
	require.NoError(t, err)

	st, err := tasks.Create(t.Context(), TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 2 }, time.Second, time.Millisecond)
	// Deliberately no RunComplete published: the run is left in flight.

	require.NoError(t, tasks.Cancel(t.Context(), st.ID, "no longer needed"))

	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusInterrupted, got.Status)
	require.Equal(t, "no longer needed", got.Error)

	require.False(t, coord.cancelAllWasCalled())
	require.Equal(t, []string{st.SessionID}, coord.canceledSessions(),
		"cancel must reach only the task's own session, not the foreground one sharing its App")
	require.Nil(t, mgr.Handle(st.ID), "the runtime must be released")
}

func TestTaskManager_CancelDefaultsReasonWhenEmpty(t *testing.T) {
	store := newTestStoreDB(t)
	_, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.AgentCoordinator.(*fakeCoordinator)

	st, err := tasks.Create(t.Context(), TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, time.Millisecond)

	require.NoError(t, tasks.Cancel(t.Context(), st.ID, ""))

	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusInterrupted, got.Status)
	require.Equal(t, "cancelled", got.Error)
}

// TestTaskManager_CancelAlreadyFinishedIsNoop proves a finished task's
// real outcome is never clobbered by a Cancel call that arrives too late.
func TestTaskManager_CancelAlreadyFinishedIsNoop(t *testing.T) {
	store := newTestStoreDB(t)
	_, tasks, parentApp := newTestTaskManager(t, store)
	coord := parentApp.AgentCoordinator.(*fakeCoordinator)

	st, err := tasks.Create(t.Context(), TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, time.Millisecond)
	publishSuccess(t, parentApp, st.SessionID)
	require.Eventually(t, func() bool {
		got, err := store.Get(t.Context(), st.ID)
		return err == nil && got.Status == StatusCompleted
	}, time.Second, time.Millisecond)

	require.NoError(t, tasks.Cancel(t.Context(), st.ID, "too late"))

	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, got.Status, "a finished task's real outcome must not be overwritten")
	require.Empty(t, coord.canceledSessions(), "no live runtime means nothing to cancel")
}

func TestTaskManager_CancelRejectsThreadID(t *testing.T) {
	store := newTestStoreDB(t)
	_, tasks, _ := newTestTaskManager(t, store)
	threadRow := seedThreadRow(t, store)
	_, err := store.SetStatus(t.Context(), threadRow.ID, SetStatusParams{Status: StatusRunning})
	require.NoError(t, err)

	err = tasks.Cancel(t.Context(), threadRow.ID, "nope")
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not a task")

	got, err := store.Get(t.Context(), threadRow.ID)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, got.Status, "a rejected call must not touch the thread's row")
}
