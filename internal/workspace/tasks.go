package workspace

import (
	"context"

	"github.com/rave-soft/sennit/internal/app/threadspawn"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/thread"
)

// -- AppWorkspace: Tasks --

// taskManager returns this workspace's *thread.TaskManager and whether one
// is attached.
func (w *AppWorkspace) taskManager() (*thread.TaskManager, bool) {
	mgr := w.app.TaskManager()
	return mgr, mgr != nil
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
