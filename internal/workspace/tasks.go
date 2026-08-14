package workspace

import (
	"context"

	"github.com/rave-soft/braid/internal/app/threadspawn"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/thread"
)

// -- AppWorkspace: Tasks --

// taskManager returns this workspace's *thread.TaskManager and whether one
// is attached. app.TaskManager() returns `any` for the same layering
// reason as threadManager (see its doc comment): internal/app cannot
// import internal/thread directly.
func (w *AppWorkspace) taskManager() (*thread.TaskManager, bool) {
	mgr, ok := w.app.TaskManager().(*thread.TaskManager)
	return mgr, ok && mgr != nil
}

func (w *AppWorkspace) SupportsTasks() bool {
	_, ok := w.taskManager()
	return ok
}

func (w *AppWorkspace) ListTasks(ctx context.Context) ([]proto.Thread, error) {
	mgr, ok := w.taskManager()
	if !ok {
		return nil, ErrTasksNotSupported
	}
	sts, err := mgr.List(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]proto.Thread, len(sts))
	for i, st := range sts {
		// A task has no workspace of its own (see TaskController's doc
		// comment), so workspaceID is always "".
		result[i] = threadspawn.ToProto(st, "")
	}
	return result, nil
}

func (w *AppWorkspace) CancelTask(ctx context.Context, id, reason string) error {
	mgr, ok := w.taskManager()
	if !ok {
		return ErrTasksNotSupported
	}
	return mgr.Cancel(ctx, id, reason)
}

// -- ClientWorkspace: Tasks --

// SupportsTasks returns the cached capability advertised by the server at
// registration (see setTasksSupported). Unlike SupportsThreads, this never
// needs a live fallback probe: TasksSupported (and the /tasks routes it
// describes) is new, so there is no older server predating the field to be
// compatible with — a workspace snapshot that omits it (nil) is simply
// unsupported, not "unknown".
func (w *ClientWorkspace) SupportsTasks() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.supportsTasks
}

// setTasksSupported updates the cached capability from a workspace
// snapshot's TasksSupported field. Callers must hold w.mu.
func (w *ClientWorkspace) setTasksSupported(supported *bool) {
	w.supportsTasks = supported != nil && *supported
}

func (w *ClientWorkspace) ListTasks(ctx context.Context) ([]proto.Thread, error) {
	return w.client.ListTasks(ctx, w.workspaceID())
}

func (w *ClientWorkspace) CancelTask(ctx context.Context, id, reason string) error {
	_, err := w.client.CancelTask(ctx, w.workspaceID(), id, reason)
	return err
}
