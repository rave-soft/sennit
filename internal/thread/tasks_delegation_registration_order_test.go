package thread_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/thread"
)

// hookedSetSessionStore wraps a real Store, running onSetSessionSuccess
// right after a successful SetSession call — the exact seam
// TestTaskManager_CreateDoesNotRegisterDelegationParentWhenStatusWriteFails
// needs to land a cancel between SetSession succeeding and the setStatus
// call that follows it.
type hookedSetSessionStore struct {
	thread.Store
	onSetSessionSuccess func()
}

func (h *hookedSetSessionStore) SetSession(ctx context.Context, id, sessionID string) (thread.Thread, error) {
	st, err := h.Store.SetSession(ctx, id, sessionID)
	if err == nil && h.onSetSessionSuccess != nil {
		h.onSetSessionSuccess()
	}
	return st, err
}

// TestTaskManager_CreateDoesNotRegisterDelegationParentWhenStatusWriteFails
// pins the ordering between registerParent and the setStatus(StatusRunning)
// call in TaskManager.Create. registerParent used to run first, on the
// theory (stated in a since-corrected comment) that doing so before the
// fallible setStatus call meant no later error path could leave a
// half-registered parent - backwards: registering first is exactly what
// left a stale DelegationParent, keyed by a child session that never
// actually started running, whenever the following setStatus call failed
// (its most common cause being the same cancelled ctx that also breaks the
// failure write - see failCreate).
func TestTaskManager_CreateDoesNotRegisterDelegationParentWhenStatusWriteFails(t *testing.T) {
	store := &hookedSetSessionStore{Store: thread.NewStoreForTest(t)}
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:    store,
		Spawner:  newFakeSpawner(t),
		RepoRoot: t.TempDir(),
	})
	shutdownManagerOnCleanup(t, mgr)
	parentApp := newTestParentApp(t)
	tasks := thread.NewTaskManagerFromManager(mgr, NewTestParentAppSpawner(parentApp), NewTestMessageService(parentApp.Messages()))

	ctx, cancel := context.WithCancel(t.Context())
	// Cancel right when SetSession succeeds - after the child session id is
	// durably attached to the task record, but before the setStatus call
	// that follows it, which is exactly the window a stale registration
	// would be created in under the old ordering.
	store.onSetSessionSuccess = func() { cancel() }

	_, err := tasks.Create(ctx, thread.TaskCreateArgs{Goal: "do the thing", ParentSessionID: "parent-sess"})
	require.Error(t, err)

	coord := parentApp.AgentCoordinator.(*fakeCoordinator)
	require.Empty(t, coord.registeredDelegationParents(),
		"a task whose setStatus write failed after SetSession must not leave a DelegationParent registered for a session that never started running")
}
