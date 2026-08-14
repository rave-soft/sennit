package thread

import (
	"testing"
	"time"

	"github.com/rave-soft/braid/internal/app"
	"github.com/stretchr/testify/require"
)

// newTestManagerWithParentApp is newTestManager, plus a parent App wired
// with a fakeCoordinator so a thread's completion delivery (see
// Manager.resolveDeliveryTarget) can actually be observed. Most of
// manager_test.go's tests have no reason to care about delivery and use
// newTestManager instead; these do.
func newTestManagerWithParentApp(t *testing.T, repo string) (*Manager, *fakeSpawner, *app.App) {
	t.Helper()
	parentApp := newTestParentApp(t)
	spawner := newFakeSpawner(t)
	mgr := NewManager(ManagerOptions{
		Store:       newTestStoreDB(t),
		Spawner:     spawner,
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
		ParentApp:   parentApp,
	})
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

	st, err := mgr.Create(t.Context(), CreateArgs{
		Name:            "alpha",
		Goal:            "do the thing",
		MergePolicy:     MergeManual,
		ParentSessionID: "parent-sess",
	})
	require.NoError(t, err)

	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, 2*time.Second))

	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, st.Status,
		"a manual-policy thread rests at completed - no merge flow to hand off to")

	parentCoord := parentApp.AgentCoordinator.(*fakeCoordinator)
	require.Eventually(t, func() bool { return len(parentCoord.deliveredCompletions()) > 0 }, time.Second, time.Millisecond)

	// Nothing else touches this session, so the count is stable once
	// observed non-empty - no need for a settling sleep before asserting
	// exactly one.
	delivered := parentCoord.deliveredCompletions()
	require.Len(t, delivered, 1)
	got := delivered[0]
	require.Equal(t, "parent-sess", got.sessionID)
	require.Equal(t, st.ID, got.completion.DelegationID)
	require.Equal(t, string(KindThread), got.completion.Kind)
	require.Equal(t, string(StatusCompleted), got.completion.Status)
	require.Equal(t, st.SessionID, got.completion.ChildSessionID)
	require.Equal(t, "finished", got.completion.ResultText)
}

// TestManager_AutoMergeThreadDeliversOnceAcrossRunAndMerge proves the
// two-terminal-moment case the plan called out specifically: an
// auto-merge thread's run finishing does NOT deliver (onAutoMerge takes
// over before lifecycle's generic delivery call is ever reached) - only
// the merge landing does, and only once.
func TestManager_AutoMergeThreadDeliversOnceAcrossRunAndMerge(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, parentApp := newTestManagerWithParentApp(t, repo)

	st, err := mgr.Create(t.Context(), CreateArgs{
		Name:            "gamma",
		Goal:            "do it",
		ParentSessionID: "parent-sess",
	})
	require.NoError(t, err)
	require.Equal(t, MergeAuto, st.MergePolicy)

	writeFile(t, st.WorktreePath, "output.txt", "auto merged\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)

	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, 2*time.Second))
	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusMerged, st.Status)

	parentCoord := parentApp.AgentCoordinator.(*fakeCoordinator)
	require.Eventually(t, func() bool { return len(parentCoord.deliveredCompletions()) > 0 }, time.Second, time.Millisecond)

	// Give a wrongly-duplicated delivery (one at run-completion, one at
	// merge) a moment to land before asserting the final count is
	// exactly one.
	time.Sleep(50 * time.Millisecond)
	delivered := parentCoord.deliveredCompletions()
	require.Len(t, delivered, 1,
		"must deliver exactly once across run-completion and the merge landing, not twice")
	require.Equal(t, string(StatusMerged), delivered[0].completion.Status,
		"the delivered event must be the merge outcome, not the run finishing mid-flight")
}

// TestManager_AutoMergeThreadConflictDeliversOnceNotAgainOnManualRetry
// goes further than the plain success case: an auto-merge thread that
// lands on a conflict delivers that outcome once, and a later manual
// Merge retry (the user resolving it by hand) must not deliver a second
// completion - its caller already observes the result synchronously
// through Merge's own return value. This is the retry hazard the plan
// flagged as the easy way to get "at most once" wrong here.
func TestManager_AutoMergeThreadConflictDeliversOnceNotAgainOnManualRetry(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, parentApp := newTestManagerWithParentApp(t, repo)

	st, err := mgr.Create(t.Context(), CreateArgs{
		Name:            "delta",
		Goal:            "do it",
		ParentSessionID: "parent-sess",
	})
	require.NoError(t, err)
	require.Equal(t, MergeAuto, st.MergePolicy)

	// The base branch and the thread branch each edit README.md,
	// guaranteeing a conflict on the automatic merge attempt.
	writeFile(t, repo, "README.md", "main version\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "edit on main")
	writeFile(t, st.WorktreePath, "README.md", "thread version\n")

	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, 2*time.Second))

	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusConflict, st.Status)

	parentCoord := parentApp.AgentCoordinator.(*fakeCoordinator)
	require.Eventually(t, func() bool { return len(parentCoord.deliveredCompletions()) > 0 }, time.Second, time.Millisecond)
	require.Len(t, parentCoord.deliveredCompletions(), 1)
	require.Equal(t, string(StatusConflict), parentCoord.deliveredCompletions()[0].completion.Status)

	// Resolve by hand and retry manually - the caller of Merge gets the
	// outcome directly from its return value.
	writeFile(t, st.WorktreePath, "README.md", "resolved version\n")
	runGit(t, st.WorktreePath, "add", "README.md")
	require.NoError(t, mgr.Merge(t.Context(), st.ID))

	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusMerged, st.Status)

	time.Sleep(50 * time.Millisecond)
	require.Len(t, parentCoord.deliveredCompletions(), 1,
		"a manual Merge retry must not deliver a second completion for the same thread")
}

// TestManager_ParentlessThreadDeliversNothing proves a thread created
// with no ParentSessionID (optional, unlike a task's required one) is a
// clean no-op for delivery: nothing is delivered anywhere, and nothing
// about the thread's own lifecycle - run, status, teardown - is left
// half-done because there was nobody to tell.
func TestManager_ParentlessThreadDeliversNothing(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, parentApp := newTestManagerWithParentApp(t, repo)

	st, err := mgr.Create(t.Context(), CreateArgs{
		Name:        "solo",
		Goal:        "do it",
		MergePolicy: MergeManual,
		// No ParentSessionID.
	})
	require.NoError(t, err)

	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, 2*time.Second))

	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, st.Status,
		"a parentless thread's own lifecycle is otherwise unaffected by having nobody to deliver to")

	time.Sleep(50 * time.Millisecond)
	parentCoord := parentApp.AgentCoordinator.(*fakeCoordinator)
	require.Empty(t, parentCoord.deliveredCompletions(),
		"a thread created with no parent session must deliver nothing")
}

// TestManager_ResolveDeliveryTarget_ThreadEdgeCases is a focused unit
// test of resolveDeliveryTarget's KindThread branch, independent of a
// full run/merge flow: no ParentApp configured, an entity with no known
// control, and an empty parent link must all report ok=false rather than
// erroring or panicking. handle is nil throughout - the KindThread
// branch never touches it (see resolveDeliveryTarget's doc comment).
func TestManager_ResolveDeliveryTarget_ThreadEdgeCases(t *testing.T) {
	repo := initRepo(t)

	t.Run("no ParentApp configured", func(t *testing.T) {
		mgr, _ := newTestManager(t, repo) // no ParentApp
		st, err := mgr.Create(t.Context(), CreateArgs{Name: "no-parent-app", Goal: "x", MergePolicy: MergeManual, ParentSessionID: "parent-sess"})
		require.NoError(t, err)
		target, parentSessionID, ok := mgr.resolveDeliveryTarget(t.Context(), nil, st)
		require.False(t, ok)
		require.Nil(t, target)
		require.Empty(t, parentSessionID)
	})

	t.Run("unknown thread id has no control", func(t *testing.T) {
		mgr, _, _ := newTestManagerWithParentApp(t, repo)
		target, parentSessionID, ok := mgr.resolveDeliveryTarget(t.Context(), nil, Thread{Delegation: Delegation{ID: "never-created", Kind: KindThread, SessionID: "sess"}})
		require.False(t, ok)
		require.Nil(t, target)
		require.Empty(t, parentSessionID)
	})

	t.Run("empty parent link", func(t *testing.T) {
		mgr, _, _ := newTestManagerWithParentApp(t, repo)
		st, err := mgr.Create(t.Context(), CreateArgs{Name: "empty-parent-link", Goal: "x", MergePolicy: MergeManual})
		require.NoError(t, err)
		target, parentSessionID, ok := mgr.resolveDeliveryTarget(t.Context(), nil, st)
		require.False(t, ok)
		require.Nil(t, target)
		require.Empty(t, parentSessionID)
	})
}
