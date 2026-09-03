package thread_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/thread"
)

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
