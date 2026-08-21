package thread_test

import (
	"context"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/thread"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// newTestTaskManagerWiredLikeProduction assembles a Manager and a
// TaskManager the way the production composition seam does: the Manager's
// ParentApp and the TaskManager's spawner Workspace are ONE shared, stable
// workspace adapter over the parent App. That shared identity is exactly
// what the domain relies on to recognize a task's workspace as its own
// parent (see lifecycle.forwardPermissions and Manager.PermissionsFor), so
// this fixture must not allocate a fresh adapter per Workspace() call the
// way a naive spawner would — that is the regression the reviewer flagged.
func newTestTaskManagerWiredLikeProduction(t *testing.T) (*thread.Manager, *thread.TaskManager, *app.App) {
	t.Helper()
	store := thread.NewStoreForTest(t)
	parentApp := newTestParentApp(t)
	parent := &testAppWorkspace{app: parentApp} // one stable adapter instance
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:     store,
		Spawner:   newFakeSpawner(t),
		RepoRoot:  t.TempDir(),
		ParentApp: parent,
	})
	tasks := thread.NewTaskManagerForTest(mgr, stableParentAppSpawner{ws: parent}, NewTestMessageService(parentApp.Messages()))
	return mgr, tasks, parentApp
}

// stableParentAppSpawner is the production ParentAppSpawner's identity
// contract, distilled: every Spawn returns a Handle whose Workspace is the
// one stable workspace adapter the Manager's ParentApp is. It stands in for
// threadspawn.NewParentAppSpawner, which an in-package test cannot import.
type stableParentAppSpawner struct {
	ws thread.Workspace
}

func (s stableParentAppSpawner) Spawn(ctx context.Context, path string) (thread.Handle, error) {
	return &stableParentAppHandle{ws: s.ws}, nil
}
func (s stableParentAppSpawner) Release(ctx context.Context, id string) error { return nil }

type stableParentAppHandle struct {
	ws thread.Workspace
}

func (h *stableParentAppHandle) ID() string                  { return "" }
func (h *stableParentAppHandle) Workspace() thread.Workspace { return h.ws }

// TestTaskManager_NoPermissionRelayForParentWorkspace is the regression
// test for a task being mis-detected as an isolated thread. A task runs
// inside its parent's own App, so its permission traffic is already on the
// stream the user watches and the relay must NOT start. With a stable
// parent-workspace identity, forwardPermissions sees
// handle.Workspace() == parentApp and returns immediately — no relay, and
// PermissionsFor answers nil so a caller falls back to its own (the
// parent's) service rather than re-routing to the very service that is
// already answering. A task mis-detected as a thread would instead start a
// relay that re-publishes the request onto the same stream, so the user
// sees the prompt twice; this test raises a request, grants it through the
// parent's own service, and asserts the stream carried exactly one copy.
func TestTaskManager_NoPermissionRelayForParentWorkspace(t *testing.T) {
	mgr, tasks, parentApp := newTestTaskManagerWiredLikeProduction(t)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "do it", ParentSessionID: "parent-sess"})
	require.NoError(t, err)

	// The task's runtime is live (a run was dispatched), so a delegation id
	// resolves to a runtime — the precondition PermissionsFor checks before
	// it can return the workspace's service.
	_, live := mgr.RuntimeForTest(st.ID)
	require.True(t, live,
		"a created task must have a live runtime for the relay decision to be reachable")

	// The task shares the parent workspace, so there is nothing to relay to
	// the parent: PermissionsFor must answer nil, never the parent service.
	require.Nil(t, mgr.PermissionsFor(st.ID),
		"a task's workspace is its own parent, so there is no separate service to route to")

	// Raise a request on the parent's own permission service, exactly as a
	// task's bash tool would (a task's handle wraps the parent App). The
	// parent's event fan-in publishes it once; a stray relay would publish
	// it a second time.
	events := parentApp.Events(t.Context())
	granted := make(chan bool, 1)
	go func() {
		ok, _ := parentApp.Permissions().Request(t.Context(), permission.CreatePermissionRequest{
			SessionID:  st.SessionID,
			ToolCallID: "call-1",
			ToolName:   "bash",
			Action:     "execute",
		})
		granted <- ok
	}()

	// Capture every permission request the stream carries until the request
	// is answered. With no relay there is exactly one.
	seen := 0
	deadline := time.After(10 * time.Second)
	// quiet is armed only once the first copy has landed, and a nil channel
	// blocks forever until then. An idle window that could also fire before
	// anything arrived would read "no duplicate" as "nothing at all" —
	// which is how this failed on a loaded CI runner, asserting 1 == 0
	// against a request that simply had not been published yet.
	var quiet <-chan time.Time
	for {
		select {
		case ev := <-events:
			if _, ok := ev.Payload.(pubsub.Event[permission.PermissionRequest]); ok {
				seen++
				// The relay (if wrongly started) re-publishes the same
				// request; grant it the moment the first copy is seen so
				// the request does not hang, then keep watching briefly for
				// a duplicate.
				parentApp.Permissions().Grant(permission.PermissionRequest{SessionID: st.SessionID})
				quiet = time.After(150 * time.Millisecond)
			}
		case <-granted:
		case <-quiet:
			require.Equal(t, 1, seen,
				"a task's request must reach the parent stream exactly once - a relay would double it")
			return
		case <-deadline:
			t.Fatalf("timed out waiting for the task's request to resolve (seen=%d)", seen)
		}
	}
}

// TestWorkspace_NilCoordinatorAdapterIsNil guards the cancel/shutdown path:
// an App that never initialized a coordinator must surface as a nil
// [Coordinator] through the domain port, not a non-nil adapter wrapping nil
// (which would panic on Cancel during shutdown). The production
// NewCoordinatorAdapter implements this (see
// internal/app/threadspawn); the in-package test adapter mirrors it, so
// this pins the contract both rely on.
func TestWorkspace_NilCoordinatorAdapterIsNil(t *testing.T) {
	a := app.NewForTest(context.Background())
	t.Cleanup(a.ShutdownForTest)
	// AgentCoordinator left nil, as app.NewForTest does.
	require.Nil(t, a.Coordinator())
	require.Nil(t, (&testAppWorkspace{app: a}).Coordinator(),
		"a nil underlying coordinator must stay nil through the domain port")
}
