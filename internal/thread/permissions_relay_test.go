package thread

import (
	"testing"
	"time"

	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// TestManager_ThreadPermissionRequestReachesTheParentStream is the
// regression test for a hang with no visible cause: a thread runs in an
// isolated App with its own permission service and its own event broker,
// and the user's TUI is subscribed to the parent workspace. A prompt
// raised inside a thread was published where nobody was listening, while
// permission.Service.Request blocked on its response channel with no
// timeout. Every thread that touched bash stopped dead and showed nothing.
//
// Drilling into the thread did not help either: a subscription only
// carries future events, and the request had already been published into
// the void.
func TestManager_ThreadPermissionRequestReachesTheParentStream(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, parent := newTestManagerWithParentApp(t, repo)
	events := parent.Events(t.Context())

	st, err := mgr.Create(t.Context(), CreateArgs{
		Name:        "asks-for-bash",
		Goal:        "implement the thing",
		MergePolicy: MergeManual,
	})
	require.NoError(t, err)

	handle := spawner.handleFor(st.WorktreePath)
	require.NotNil(t, handle)

	// Raise a request inside the thread's own workspace, exactly as its
	// bash tool would. Request blocks until answered, so it runs in its
	// own goroutine.
	go func() {
		_, _ = handle.App().Permissions.Request(t.Context(), permission.CreatePermissionRequest{
			SessionID:   st.SessionID,
			ToolCallID:  "call-1",
			ToolName:    "bash",
			Description: "run the tests",
			Action:      "execute",
			Path:        st.WorktreePath,
		})
	}()

	req := awaitPermissionRequest(t, events)
	require.Equal(t, "bash", req.ToolName,
		"a prompt raised inside a thread must reach the stream the user is watching")
}

// TestManager_ForwardedPermissionCarriesItsDelegation: the relayed prompt
// has to say which thread wants it. Several threads can be running at
// once, each against its own worktree, so an unattributed prompt asks the
// user to approve a command without saying what it belongs to - and the
// answer could not be routed back either.
func TestManager_ForwardedPermissionCarriesItsDelegation(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, parent := newTestManagerWithParentApp(t, repo)
	events := parent.Events(t.Context())

	st, err := mgr.Create(t.Context(), CreateArgs{
		Name:        "named-thread",
		Goal:        "implement the thing",
		MergePolicy: MergeManual,
	})
	require.NoError(t, err)

	handle := spawner.handleFor(st.WorktreePath)
	require.NotNil(t, handle)

	// The delegation ref rides the run's context, which is why the tag is
	// applied on every dispatch path (see lifecycle.withDelegation). Here
	// the request is raised directly, so it carries the same ctx the run
	// would have.
	ctx := permission.WithDelegation(t.Context(), permission.DelegationRef{
		ID: st.ID, Name: st.Name, Kind: string(st.Kind),
	})
	go func() {
		_, _ = handle.App().Permissions.Request(ctx, permission.CreatePermissionRequest{
			SessionID:  st.SessionID,
			ToolCallID: "call-1",
			ToolName:   "bash",
			Action:     "execute",
			Path:       st.WorktreePath,
		})
	}()

	req := awaitPermissionRequest(t, events)
	require.Equal(t, st.ID, req.Delegation.ID)
	require.Equal(t, "named-thread", req.Delegation.Name)
	require.Equal(t, string(KindThread), req.Delegation.Kind)
}

// TestManager_PermissionsForRoutesToTheThreadThatIsWaiting covers the
// return path. The parent displays the prompt but does not hold it:
// answering against the parent's own service would find no such request
// and silently do nothing, leaving the thread blocked on the prompt the
// user just answered.
func TestManager_PermissionsForRoutesToTheThreadThatIsWaiting(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, parent := newTestManagerWithParentApp(t, repo)
	events := parent.Events(t.Context())

	st, err := mgr.Create(t.Context(), CreateArgs{
		Name:        "waiting-thread",
		Goal:        "implement the thing",
		MergePolicy: MergeManual,
	})
	require.NoError(t, err)

	handle := spawner.handleFor(st.WorktreePath)
	require.NotNil(t, handle)

	ctx := permission.WithDelegation(t.Context(), permission.DelegationRef{
		ID: st.ID, Name: st.Name, Kind: string(st.Kind),
	})
	granted := make(chan bool, 1)
	go func() {
		ok, _ := handle.App().Permissions.Request(ctx, permission.CreatePermissionRequest{
			SessionID:  st.SessionID,
			ToolCallID: "call-1",
			ToolName:   "bash",
			Action:     "execute",
			Path:       st.WorktreePath,
		})
		granted <- ok
	}()

	req := awaitPermissionRequest(t, events)

	svc := mgr.PermissionsFor(req.Delegation.ID)
	require.NotNil(t, svc, "a live thread's permission service must be resolvable from its delegation id")
	require.NotSame(t, parent.Permissions, svc, "and it must not be the parent's own")
	require.True(t, svc.Grant(req), "granting must resolve the pending request")

	select {
	case ok := <-granted:
		require.True(t, ok, "the thread's blocked tool call must be released by the parent's answer")
	case <-time.After(5 * time.Second):
		t.Fatal("the thread stayed blocked after its permission request was granted")
	}
}

// TestManager_PermissionsForUnknownDelegation: nothing to route means the
// caller falls back to its own service, so this must answer nil rather
// than guess.
func TestManager_PermissionsForUnknownDelegation(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	require.Nil(t, mgr.PermissionsFor(""))
	require.Nil(t, mgr.PermissionsFor("no-such-delegation"))
}

// awaitPermissionRequest pulls the next permission request off a parent
// workspace's event stream. The stream carries every kind of app event, so
// unrelated ones are skipped rather than failed on.
func awaitPermissionRequest(t *testing.T, events <-chan pubsub.Event[any]) permission.PermissionRequest {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-events:
			if inner, ok := ev.Payload.(pubsub.Event[permission.PermissionRequest]); ok {
				return inner.Payload
			}
		case <-deadline:
			t.Fatal("no permission request reached the parent workspace's event stream")
		}
	}
}
