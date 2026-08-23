package thread_test

import (
	"testing"

	"github.com/rave-soft/sennit/internal/thread"

	"github.com/stretchr/testify/require"
)

// TestManager_ResolveDeliveryTarget_SurvivesRestart is the core restart
// stand-in this feature exists for: a thread's parent link, read back by a
// second, freshly-constructed Manager sharing only the Store and the
// ParentApp with the first (the closest honest simulation of "the process
// restarted, a new in-memory Manager/lifecycle came up, the SQLite file
// and the running parentApp did not"), must still resolve correctly. The
// whole point of persisting ParentSessionID on the row is that this no
// longer depends on any in-memory state the original Create stashed.
func TestManager_ResolveDeliveryTarget_SurvivesRestart(t *testing.T) {
	repo := initRepo(t)
	store := thread.NewStoreForTest(t)
	parentApp := newTestParentApp(t)

	mgr1 := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     newFakeSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
		ParentApp:   &testAppWorkspace{app: parentApp},
	})
	shutdownManagerOnCleanup(t, mgr1)
	st, err := mgr1.Create(t.Context(), thread.CreateArgs{
		Name:            "restart-thread",
		Goal:            "do it",
		MergePolicy:     thread.MergeManual,
		ParentSessionID: "parent-sess",
	})
	require.NoError(t, err)

	mgr2 := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     newFakeSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
		ParentApp:   &testAppWorkspace{app: parentApp},
	})
	shutdownManagerOnCleanup(t, mgr2)
	fresh, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)

	target, parentSessionID, ok := mgr2.ResolveDeliveryTargetForTest(t.Context(), nil, fresh)
	require.True(t, ok)
	require.Same(t, parentApp, target.(*testAppWorkspace).app)
	require.Equal(t, "parent-sess", parentSessionID)
}

// TestManager_Send_ReregistersDelegationParentForResumedThread proves the
// "ask_parent from a resumed delegation" half of the fix: resuming a
// thread via Send on a fresh Manager (standing in for a process restart)
// re-registers its DelegationParent on the freshly-spawned workspace's own
// coordinator, using the persisted ParentSessionID - not any in-memory
// field, since the fresh Manager's lifecycle never saw the original
// Create.
func TestManager_Send_ReregistersDelegationParentForResumedThread(t *testing.T) {
	repo := initRepo(t)
	store := thread.NewStoreForTest(t)
	parentApp := newTestParentApp(t)

	mgr1 := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     newFakeSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
		ParentApp:   &testAppWorkspace{app: parentApp},
	})
	shutdownManagerOnCleanup(t, mgr1)
	st, err := mgr1.Create(t.Context(), thread.CreateArgs{
		Name:            "resume-thread",
		Goal:            "do it",
		MergePolicy:     thread.MergeManual,
		ParentSessionID: "parent-sess",
	})
	require.NoError(t, err)

	// A fresh Manager, its own spawner standing in for a brand-new
	// process: mgr1's lifecycle/controls and the DelegationParent it
	// registered on the old spawned app are both irrelevant here.
	spawner2 := newFakeSpawner(t)
	mgr2 := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     spawner2,
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
		ParentApp:   &testAppWorkspace{app: parentApp},
	})
	shutdownManagerOnCleanup(t, mgr2)

	require.NoError(t, sendErr(mgr2.Send(t.Context(), st.ID, "resume message")))

	ownCoord := spawner2.appFor(st.WorktreePath).AgentCoordinator.(*fakeCoordinator)
	registered := ownCoord.registeredDelegationParents()
	require.Len(t, registered, 1)
	got := registered[0]
	require.Equal(t, st.SessionID, got.sessionID)
	require.Same(t, parentApp.AgentCoordinator, got.parent.Parent.(*testCoordinatorAdapter).inner)
	require.Equal(t, "parent-sess", got.parent.ParentSessionID)
	require.Equal(t, st.ID, got.parent.DelegationID)
	require.Equal(t, string(thread.KindThread), got.parent.Kind)
	require.Equal(t, st.Name, got.parent.Name)
}

// TestTaskManager_Send_ReregistersDelegationParentForResumedTask is the
// task-kind counterpart of the Manager.Send test above: a fresh
// TaskManager (sharing a fresh Manager's lifecycle/store the way
// NewTaskManager requires) resuming a task re-registers its
// DelegationParent, reading the persisted ParentSessionID off the row
// rather than any in-memory control.
func TestTaskManager_Send_ReregistersDelegationParentForResumedTask(t *testing.T) {
	store := thread.NewStoreForTest(t)
	parentApp1 := newTestParentApp(t)
	mgr1 := thread.NewManager(thread.ManagerOptions{Store: store, Spawner: newFakeSpawner(t), RepoRoot: t.TempDir()})
	shutdownManagerOnCleanup(t, mgr1)
	tasks1 := thread.NewTaskManagerFromManager(mgr1, NewTestParentAppSpawner(parentApp1), NewTestMessageService(parentApp1.Messages()))

	st, err := tasks1.Create(t.Context(), thread.TaskCreateArgs{Goal: "do it", ParentSessionID: "parent-sess"})
	require.NoError(t, err)

	// A fresh Manager/TaskManager pair sharing only the store, standing in
	// for a restarted process: mgr1's lifecycle/controls and parentApp1's
	// registration are both gone.
	parentApp2 := newTestParentApp(t)
	mgr2 := thread.NewManager(thread.ManagerOptions{Store: store, Spawner: newFakeSpawner(t), RepoRoot: t.TempDir()})
	shutdownManagerOnCleanup(t, mgr2)
	tasks2 := thread.NewTaskManagerFromManager(mgr2, NewTestParentAppSpawner(parentApp2), NewTestMessageService(parentApp2.Messages()))

	require.NoError(t, sendErr(tasks2.Send(t.Context(), st.ID, "resume message")))

	coord := parentApp2.AgentCoordinator.(*fakeCoordinator)
	registered := coord.registeredDelegationParents()
	require.Len(t, registered, 1)
	got := registered[0]
	require.Equal(t, st.SessionID, got.sessionID)
	require.Same(t, parentApp2.AgentCoordinator, got.parent.Parent.(*testCoordinatorAdapter).inner)
	require.Equal(t, "parent-sess", got.parent.ParentSessionID)
	require.Equal(t, st.ID, got.parent.DelegationID)
	require.Equal(t, string(thread.KindTask), got.parent.Kind)
	require.Equal(t, st.Name, got.parent.Name)
}

// TestManager_ParentlessThread_StaysParentlessAcrossRestart proves a
// thread created with no ParentSessionID stays parentless across the same
// fresh-Manager restart simulation: delivery still resolves ok=false, and
// resuming it via Send registers no DelegationParent at all - no invented
// parent.
func TestManager_ParentlessThread_StaysParentlessAcrossRestart(t *testing.T) {
	repo := initRepo(t)
	store := thread.NewStoreForTest(t)
	parentApp := newTestParentApp(t)

	mgr1 := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     newFakeSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
		ParentApp:   &testAppWorkspace{app: parentApp},
	})
	shutdownManagerOnCleanup(t, mgr1)
	st, err := mgr1.Create(t.Context(), thread.CreateArgs{
		Name:        "restart-solo",
		Goal:        "do it",
		MergePolicy: thread.MergeManual,
		// No ParentSessionID.
	})
	require.NoError(t, err)

	spawner2 := newFakeSpawner(t)
	mgr2 := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     spawner2,
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
		ParentApp:   &testAppWorkspace{app: parentApp},
	})
	shutdownManagerOnCleanup(t, mgr2)

	fresh, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	_, _, ok := mgr2.ResolveDeliveryTargetForTest(t.Context(), nil, fresh)
	require.False(t, ok, "a thread with no recorded parent must not resolve a delivery target after restart")

	require.NoError(t, sendErr(mgr2.Send(t.Context(), st.ID, "resume message")))
	ownCoord := spawner2.appFor(st.WorktreePath).AgentCoordinator.(*fakeCoordinator)
	require.Empty(t, ownCoord.registeredDelegationParents(),
		"a parentless thread resumed after restart must register nothing")
}
