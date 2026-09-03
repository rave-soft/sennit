package thread_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/thread"
)

// TestManager_CreateInheritsAutoApprovalFromApprovedParent is the thread
// analogue of TestTaskManager_CreateInheritsAutoApprovalFromApprovedParent
// above. A thread's isolated workspace (see ManagerOptions.ParentApp) has
// a permission.Service wholly separate from its parent's — fakeSpawner
// spawns a real app.NewForTest App per thread, distinct from parentApp —
// so before this propagation existed, a thread dispatched from a
// headless run's auto-approved session ran its goal against a fresh
// service nobody had granted anything, and its first permission request
// blocked forever with no UI to answer it.
func TestManager_CreateInheritsAutoApprovalFromApprovedParent(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, parentApp := newTestManagerWithParentApp(t, repo)

	parentApp.Permissions().AutoApproveSession("parent-sess")

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:            "alpha",
		Goal:            "do the thing",
		MergePolicy:     thread.MergeManual,
		ParentSessionID: "parent-sess",
	})
	require.NoError(t, err)
	require.NotEmpty(t, st.SessionID)

	threadPerms := spawner.appFor(st.WorktreePath).Permissions()
	allowed, err := threadPerms.Request(t.Context(), permission.CreatePermissionRequest{
		SessionID:  st.SessionID,
		ToolCallID: "call-1",
		ToolName:   "bash",
		Action:     "execute",
		Path:       t.TempDir(),
	})
	require.NoError(t, err)
	require.True(t, allowed, "a thread dispatched under an auto-approved parent session must have its requests granted without a prompt")
}

// TestManager_CreateDoesNotInheritAutoApprovalFromOrdinaryParent pins the
// other half: a thread dispatched from a session that was never
// auto-approved must still raise a real prompt against its own,
// independent permission service — nothing here widens that service's
// own SkipRequests or otherwise grants it anything beyond the one
// session-scoped grant under test.
func TestManager_CreateDoesNotInheritAutoApprovalFromOrdinaryParent(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, _ := newTestManagerWithParentApp(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:            "alpha",
		Goal:            "do the thing",
		MergePolicy:     thread.MergeManual,
		ParentSessionID: "ordinary-parent-sess",
	})
	require.NoError(t, err)
	require.NotEmpty(t, st.SessionID)

	threadPerms := spawner.appFor(st.WorktreePath).Permissions()
	events := threadPerms.Subscribe(t.Context())
	granted := make(chan bool, 1)
	go func() {
		ok, _ := threadPerms.Request(t.Context(), permission.CreatePermissionRequest{
			SessionID:  st.SessionID,
			ToolCallID: "call-1",
			ToolName:   "bash",
			Action:     "execute",
			Path:       t.TempDir(),
		})
		granted <- ok
	}()

	var req permission.PermissionRequest
	select {
	case evt := <-events:
		req = evt.Payload
	case <-time.After(5 * time.Second):
		t.Fatal("the thread's permission request never reached its own service")
	}
	require.Equal(t, st.SessionID, req.SessionID)

	require.True(t, threadPerms.Deny(req))
	select {
	case ok := <-granted:
		require.False(t, ok, "a thread under an ordinary parent session must still be denyable, never auto-granted")
	case <-time.After(5 * time.Second):
		t.Fatal("the thread's request never resolved")
	}
}

// TestManager_SendReGrantsAutoApprovalAfterRuntimeRelease is the regression
// test for G1: Create's grant lives in the permission.Service of the App
// spawned for the thread's run, and that App is torn down every time the
// run completes (finalizeRunComplete -> releaseRuntime ->
// LocalSpawner.Release -> App.Shutdown). Before registerThreadParent
// carried the grant, a headless "resolve via Send and Merge again" cycle —
// exactly what merge.go documents for a conflict — respawned a fresh App
// with a fresh, ungranted permission.Service, so the follow-up's first
// permission request blocked forever with no UI to answer it. Send must
// re-grant on every respawn, not just at Create.
func TestManager_SendReGrantsAutoApprovalAfterRuntimeRelease(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, parentApp := newTestManagerWithParentApp(t, repo)

	parentApp.Permissions().AutoApproveSession("parent-sess")

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:            "alpha",
		Goal:            "do the thing",
		MergePolicy:     thread.MergeManual,
		ParentSessionID: "parent-sess",
	})
	require.NoError(t, err)

	// Complete the run so the runtime — and the App carrying the grant —
	// is released, mirroring the respawn that follows an interrupted or
	// merge-blocked headless thread being resumed.
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))

	_, err = mgr.Send(t.Context(), st.ID, "keep going")
	require.NoError(t, err)

	respawnedPerms := spawner.appFor(st.WorktreePath).Permissions()
	require.True(t, respawnedPerms.IsAutoApproveSession(st.SessionID),
		"a thread respawned by Send must still carry the auto-approval grant its parent session held")
}

// TestManager_SendDoesNotGrantAutoApprovalFromOrdinaryParent pins the other
// half: a thread whose parent session was never auto-approved must not
// come back from a respawn with a grant it was never given.
func TestManager_SendDoesNotGrantAutoApprovalFromOrdinaryParent(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, _ := newTestManagerWithParentApp(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:            "alpha",
		Goal:            "do the thing",
		MergePolicy:     thread.MergeManual,
		ParentSessionID: "ordinary-parent-sess",
	})
	require.NoError(t, err)

	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))

	_, err = mgr.Send(t.Context(), st.ID, "keep going")
	require.NoError(t, err)

	respawnedPerms := spawner.appFor(st.WorktreePath).Permissions()
	require.False(t, respawnedPerms.IsAutoApproveSession(st.SessionID),
		"a thread under an ordinary parent session must not be auto-approved after a respawn")
}

// TestTaskManager_CreateInheritsAutoApprovalFromApprovedParent is the
// regression test for the headless-delegation deadlock: a non-interactive
// run auto-approves its own session (permission.Service.AutoApproveSession),
// but a delegation launched from it used to run under a fresh child session
// id that carried no such grant, so its first permission request blocked
// forever with nothing able to answer it. Create must now extend the same
// grant to the child session it creates, whenever the parent already has
// it.
func TestTaskManager_CreateInheritsAutoApprovalFromApprovedParent(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)

	parentApp.Permissions().AutoApproveSession("parent-sess")

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	require.NotEmpty(t, st.SessionID)

	// Auto-approval resolves Request synchronously with no prompt raised,
	// so a request that returns granted here (rather than blocking) proves
	// the child session inherited the grant.
	allowed, err := parentApp.Permissions().Request(t.Context(), permission.CreatePermissionRequest{
		SessionID:  st.SessionID,
		ToolCallID: "call-1",
		ToolName:   "bash",
		Action:     "execute",
		Path:       t.TempDir(),
	})
	require.NoError(t, err)
	require.True(t, allowed, "a delegation under an auto-approved session must have its requests granted without a prompt")
}

// TestTaskManager_CreateDoesNotInheritAutoApprovalFromOrdinaryParent pins
// the other half of the property above: nothing here reintroduces the
// blanket auto-approval that TestAgenticFetchSubAgentView_OutsideWorkdirRequiresPermission
// (internal/agent) guards against. A delegation launched from a session
// that was never auto-approved must still raise a real prompt.
func TestTaskManager_CreateDoesNotInheritAutoApprovalFromOrdinaryParent(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "ordinary-parent-sess"})
	require.NoError(t, err)
	require.NotEmpty(t, st.SessionID)

	events := parentApp.Events(t.Context())
	granted := make(chan bool, 1)
	go func() {
		ok, _ := parentApp.Permissions().Request(t.Context(), permission.CreatePermissionRequest{
			SessionID:  st.SessionID,
			ToolCallID: "call-1",
			ToolName:   "bash",
			Action:     "execute",
			Path:       t.TempDir(),
		})
		granted <- ok
	}()

	req := awaitPermissionRequest(t, events)
	require.Equal(t, st.SessionID, req.SessionID)

	require.True(t, parentApp.Permissions().Deny(req))
	select {
	case ok := <-granted:
		require.False(t, ok, "a delegation under an ordinary session must still be denyable, never auto-granted")
	case <-time.After(5 * time.Second):
		t.Fatal("the delegation's request never resolved")
	}
}

// TestTaskManager_CreateInheritsAutoApprovalAcrossNestedDelegation covers a
// sub-agent that delegates again: the second-level child's own parent is
// the first-level child, whose grant Create itself installed rather than
// one the caller of Create ever set directly. The propagation must chain
// through that just as it does from the top-level session.
func TestTaskManager_CreateInheritsAutoApprovalAcrossNestedDelegation(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)

	parentApp.Permissions().AutoApproveSession("parent-sess")

	first, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "delegate once", ParentSessionID: "parent-sess"})
	require.NoError(t, err)

	second, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "delegate again", ParentSessionID: first.SessionID})
	require.NoError(t, err)

	allowed, err := parentApp.Permissions().Request(t.Context(), permission.CreatePermissionRequest{
		SessionID:  second.SessionID,
		ToolCallID: "call-1",
		ToolName:   "bash",
		Action:     "execute",
		Path:       t.TempDir(),
	})
	require.NoError(t, err)
	require.True(t, allowed, "a nested delegation must inherit auto-approval through its immediate parent")
}
