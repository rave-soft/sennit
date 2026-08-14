package thread

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

// TaskCreateArgs holds the inputs to [TaskManager.Create].
type TaskCreateArgs struct {
	// Goal is the task's prompt, dispatched immediately: unlike a thread,
	// a task has no worktree to rest idle in, so there is no goal-less
	// path the way [CreateArgs.Goal] has for [Manager.Create].
	Goal string
	// ParentSessionID is the session the task's own child session nests
	// under, the same relationship [Manager.Create] establishes for a
	// thread's child session via Sessions.CreateTaskSession. Required: a
	// task with no parent is not a task, just an orphaned session.
	ParentSessionID string
}

// TaskManager drives the task delegation kind: the same admission,
// per-entity serialization, run dispatch, status transitions, recovery,
// and shutdown as [Manager]'s threads, minus the git worktree/merge
// overlay. A task runs inside its parent workspace's own App instead of
// an isolated one it spawns — see [ParentAppSpawner] — which is the
// entire reason this kind exists: it starts cheaply where a thread
// cannot.
//
// TaskManager is deliberately a sibling of [Manager], not a method on it:
// Manager's public surface (List, Wait, Merge, and its CreateArgs'
// git-specific fields) is stated in terms of threads, and bolting a
// worktree-less kind onto it would either strain that surface or require
// every thread-only method to guard against a task ID. What TaskManager
// shares with Manager is the [lifecycle] underneath both — see
// [NewTaskManager].
type TaskManager struct {
	store   Store
	spawner Spawner
	ctx     context.Context
	lc      *lifecycle
}

// NewTaskManager constructs a TaskManager. lc and ctx must be an existing
// [Manager]'s own m.lc and m.ctx (both unexported, so this can only be
// called from within package thread), not freshly constructed ones: a
// separate lifecycle would give tasks their own controls map, worker
// group, and recovery sweep, splitting exactly the machinery this package
// exists to share, and a separate ctx would mean Manager.Shutdown's
// cancellation never reaches a task's in-flight run. spawner should be a
// [ParentAppSpawner] wrapping the workspace's own App.
func NewTaskManager(store Store, spawner Spawner, lc *lifecycle, ctx context.Context) *TaskManager {
	return &TaskManager{store: store, spawner: spawner, ctx: ctx, lc: lc}
}

// Create records a task, gives it a child session under
// args.ParentSessionID, and dispatches args.Goal immediately in the
// task's bound runtime (the parent App). It returns once the task is
// running; it does not wait for the run to finish — subscribe to the
// shared lifecycle's events (via a [Manager] sharing it, [Manager.Wait])
// for that.
//
// The task's Name is generated (task-<uuid>), not caller-supplied: unlike
// a thread, a task has no user-chosen name, and several nameless tasks
// would otherwise collide on the store's UNIQUE(project_path, name).
func (t *TaskManager) Create(ctx context.Context, args TaskCreateArgs) (Thread, error) {
	done, err := t.lc.beginOp()
	if err != nil {
		return Thread{}, err
	}
	defer done()
	if args.Goal == "" {
		return Thread{}, fmt.Errorf("thread: task goal is required")
	}
	if args.ParentSessionID == "" {
		return Thread{}, fmt.Errorf("thread: task requires a parent session")
	}

	st, err := t.store.Create(ctx, CreateParams{
		Name: "task-" + uuid.NewString(),
		Goal: args.Goal,
		Kind: KindTask,
	})
	if err != nil {
		return Thread{}, fmt.Errorf("thread: create task record: %w", err)
	}
	t.lc.publish(EventCreated, st)

	// The task is resolvable from here on. Nothing tears a task down yet
	// (this step does not wire task removal), but startRun's contract
	// requires the caller hold the entity's opMu across installation
	// regardless, so this mirrors Manager.Create's guard for when that
	// changes.
	c := t.lc.control(st.ID)
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	removed := c.removed
	c.mu.Unlock()
	if removed {
		return Thread{}, fmt.Errorf("thread: task %q was removed during creation", st.ID)
	}

	handle, err := t.spawner.Spawn(t.ctx, "")
	if err != nil {
		return Thread{}, t.failCreate(ctx, st, err)
	}
	// This call owns handle until startRun installs it as the shared
	// runtime; release it on every earlier exit. ParentAppSpawner.Release
	// is a no-op, but a future Spawner for this kind might not be.
	owned := true
	defer func() {
		if owned {
			_ = t.spawner.Release(ctx, handle.ID())
		}
	}()

	sess, err := handle.App().Sessions.CreateTaskSession(ctx, uuid.NewString(), args.ParentSessionID, args.Goal)
	if err != nil {
		return Thread{}, t.failCreate(ctx, st, err)
	}

	st, err = t.store.SetSession(ctx, st.ID, sess.ID)
	if err != nil {
		return Thread{}, t.failCreate(ctx, st, err)
	}

	st, err = t.lc.setStatus(ctx, st.ID, StatusRunning, "", "", 0)
	if err != nil {
		return Thread{}, err
	}
	t.lc.startRun(t.ctx, handle, t.spawner, st.ID, st.SessionID, args.Goal)
	owned = false // Ownership transferred to the shared runtime state.

	return st, nil
}

// failCreate records cause as the task's terminal failure and returns it
// to Create's caller.
func (t *TaskManager) failCreate(ctx context.Context, st Thread, cause error) error {
	if _, err := t.lc.setStatus(ctx, st.ID, StatusFailed, cause.Error(), "", 0); err != nil {
		slog.Error("thread: recording task create failure failed", "task", st.ID, "error", err)
	}
	return cause
}
