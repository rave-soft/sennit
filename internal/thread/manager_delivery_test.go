package thread_test

import (
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/thread"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/stretchr/testify/require"
)

// newTestManagerWithParentApp is newTestManager, plus a parent App wired
// with a fakeCoordinator so a thread's completion delivery (see
// Manager.resolveDeliveryTarget) can actually be observed. Most of
// manager_test.go's tests have no reason to care about delivery and use
// newTestManager instead; these do.
func newTestManagerWithParentApp(t *testing.T, repo string) (*thread.Manager, *fakeSpawner, *app.App) {
	t.Helper()
	parentApp := newTestParentApp(t)
	spawner := newFakeSpawner(t)
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       thread.NewStoreForTest(t),
		Spawner:     spawner,
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
		ParentApp:   &testAppWorkspace{app: parentApp},
	})
	shutdownManagerOnCleanup(t, mgr)
	return mgr, spawner, parentApp
}

// TestManager_ManualPolicyThreadDeliversCompletionToParentOnce proves a
// manual-policy thread's own run completing is its terminal event: it
// reaches the parent's completion inbox exactly once, through the same
// generic path a task's completion already used (see
// lifecycle.handleRunComplete's delivery call).
func TestManager_ManualPolicyThreadDeliversCompletionToParentOnce(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, parentApp := newTestManagerWithParentApp(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:            "alpha",
		Goal:            "do the thing",
		ParentSessionID: "parent-sess",
	})
	require.NoError(t, err)
	writeFile(t, st.WorktreePath, "result.txt", "kept\n")

	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))

	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusCompleted, st.Status,
		"a retained thread rests at completed after cleanup safety is decided")

	parentCoord := parentApp.Coordinator().(*fakeCoordinator)
	require.Eventually(t, func() bool { return len(parentCoord.deliveredCompletions()) > 0 }, eventuallyTimeout, eventuallyTick)

	// Nothing else touches this session, so the count is stable once
	// observed non-empty - no need for a settling sleep before asserting
	// exactly one.
	delivered := parentCoord.deliveredCompletions()
	require.Len(t, delivered, 1)
	got := delivered[0]
	require.Equal(t, "parent-sess", got.sessionID)
	require.Equal(t, st.ID, got.completion.DelegationID)
	require.Equal(t, string(thread.KindThread), got.completion.Kind)
	require.Equal(t, string(thread.StatusCompleted), got.completion.Status)
	require.Equal(t, st.SessionID, got.completion.ChildSessionID)
	require.Equal(t, "finished", got.completion.ResultText)
}

func TestManager_ParentlessThreadDeliversNothing(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, parentApp := newTestManagerWithParentApp(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name: "solo",
		Goal: "do it",
		// No ParentSessionID.
	})
	require.NoError(t, err)
	writeFile(t, st.WorktreePath, "result.txt", "kept\n")

	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))

	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusCompleted, st.Status,
		"a parentless thread's own lifecycle is otherwise unaffected by having nobody to deliver to")

	time.Sleep(50 * time.Millisecond)
	parentCoord := parentApp.Coordinator().(*fakeCoordinator)
	require.Empty(t, parentCoord.deliveredCompletions(),
		"a thread created with no parent session must deliver nothing")
}

// TestManager_CreateWithParentRegistersDelegationParent proves a thread
// created with a ParentSessionID (and a Manager built with ParentApp set)
// registers its child session on its OWN coordinator (whose dispatcher
// runs the thread's own turns), but with Parent resolving to the
// Manager's parentApp coordinator — never the thread's own, wholly
// isolated one — mirroring resolveDeliveryTarget's KindThread branch.
func TestManager_CreateWithParentRegistersDelegationParent(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, parentApp := newTestManagerWithParentApp(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:            "with-parent",
		Goal:            "do the thing",
		ParentSessionID: "parent-sess",
	})
	require.NoError(t, err)

	ownCoord := spawner.appFor(st.WorktreePath).Coordinator().(*fakeCoordinator)
	registered := ownCoord.registeredDelegationParents()
	require.Len(t, registered, 1)
	got := registered[0]

	require.Equal(t, st.SessionID, got.sessionID,
		"a thread's parent must be registered under its own child session id")
	require.Equal(t, parentApp.Coordinator(), got.parent.Parent.(*testCoordinatorAdapter).inner,
		"a thread's Parent must resolve to the Manager's parentApp coordinator, not its own isolated one")
	require.Equal(t, "parent-sess", got.parent.ParentSessionID)
	require.Equal(t, st.ID, got.parent.DelegationID)
	require.Equal(t, string(thread.KindThread), got.parent.Kind)
	require.Equal(t, st.Name, got.parent.Name)
	require.Equal(t, 0, got.parent.Depth)
}

// TestManager_CreateWithoutParentRegistersNothing proves a thread created
// with no ParentSessionID (the CLI's default, no-parent case) registers
// no delegation parent at all — the parentless case a later ask_parent
// tool must fail cleanly on.
func TestManager_CreateWithoutParentRegistersNothing(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, _ := newTestManagerWithParentApp(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name: "no-parent",
		Goal: "do the thing",
		// No ParentSessionID.
	})
	require.NoError(t, err)

	ownCoord := spawner.appFor(st.WorktreePath).Coordinator().(*fakeCoordinator)
	require.Empty(t, ownCoord.registeredDelegationParents(),
		"a thread created with no parent session must register nothing")
}

// TestManager_ResolveDeliveryTarget_ThreadEdgeCases is a focused unit
// test of resolveDeliveryTarget's KindThread branch, independent of a
// full completion flow: no ParentApp configured, an entity with no known
// control, and an empty parent link must all report ok=false rather than
// erroring or panicking. handle is nil throughout - the KindThread
// branch never touches it (see resolveDeliveryTarget's doc comment).
func TestManager_ResolveDeliveryTarget_ThreadEdgeCases(t *testing.T) {
	repo := initRepo(t)

	t.Run("no ParentApp configured", func(t *testing.T) {
		mgr, _ := newTestManager(t, repo) // no ParentApp
		st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "no-parent-app", Goal: "x", ParentSessionID: "parent-sess"})
		require.NoError(t, err)
		target, parentSessionID, ok := mgr.ResolveDeliveryTargetForTest(t.Context(), nil, st)
		require.False(t, ok)
		require.Nil(t, target)
		require.Empty(t, parentSessionID)
	})

	t.Run("unknown thread id has no control", func(t *testing.T) {
		mgr, _, _ := newTestManagerWithParentApp(t, repo)
		target, parentSessionID, ok := mgr.ResolveDeliveryTargetForTest(t.Context(), nil, thread.Thread{Delegation: thread.Delegation{ID: "never-created", Kind: thread.KindThread, SessionID: "sess"}})
		require.False(t, ok)
		require.Nil(t, target)
		require.Empty(t, parentSessionID)
	})

	t.Run("empty parent link", func(t *testing.T) {
		mgr, _, _ := newTestManagerWithParentApp(t, repo)
		st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "empty-parent-link", Goal: "x"})
		require.NoError(t, err)
		target, parentSessionID, ok := mgr.ResolveDeliveryTargetForTest(t.Context(), nil, st)
		require.False(t, ok)
		require.Nil(t, target)
		require.Empty(t, parentSessionID)
	})
}
