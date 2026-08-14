package tools

import "context"

// TaskCreateArgs mirrors internal/thread.TaskCreateArgs. It is declared
// here, rather than imported, for the same reason ThreadCreateArgs is —
// see that type's doc comment.
type TaskCreateArgs struct {
	Goal            string
	ParentSessionID string
}

// TaskInfo is the minimal shape a background delegation returns
// immediately: enough to reference the task later (poll it or be
// notified), not the full delegation record. Mirrors the fields of
// internal/thread.Thread the built-in agent tool's background mode
// actually needs.
type TaskInfo struct {
	ID        string
	SessionID string
	Status    string
}

// TaskManager is the subset of internal/thread.TaskManager's API the
// built-in agent tool's background mode needs: create a task and learn
// its id, child session, and status. Kept deliberately narrower than
// ThreadManager — no list/get/cancel yet, since nothing here consumes
// them and their shape may differ once something does.
type TaskManager interface {
	Create(ctx context.Context, args TaskCreateArgs) (TaskInfo, error)
}
