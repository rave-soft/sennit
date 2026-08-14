package workspace

import (
	"context"

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
		result[i] = thread.ToProto(st, "")
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

// SupportsTasks, ListTasks, and CancelTask have no server-side route yet
// (out of scope for this step — this is the read/wrapper plumbing on the
// AppWorkspace side only), so ClientWorkspace deliberately reports no task
// support rather than attempting HTTP calls that don't exist.
func (w *ClientWorkspace) SupportsTasks() bool { return false }

func (w *ClientWorkspace) ListTasks(ctx context.Context) ([]proto.Thread, error) {
	return nil, ErrTasksNotSupported
}

func (w *ClientWorkspace) CancelTask(ctx context.Context, id, reason string) error {
	return ErrTasksNotSupported
}
