package tools

import "context"

// TaskCreateArgs mirrors internal/thread.TaskCreateArgs. It is declared
// here, rather than imported, for the same reason ThreadCreateArgs is —
// see that type's doc comment.
type TaskCreateArgs struct {
	Goal            string
	ParentSessionID string
}

// TaskInfo mirrors the fields of internal/thread.Thread the task_* tools
// need: enough to list, report on, and reference a task later, but none
// of the git-worktree fields Thread carries for the thread overlay (a
// task has no worktree, branch, or merge policy).
type TaskInfo struct {
	ID            string
	Goal          string
	SessionID     string
	Status        string
	ResultSummary string
	Error         string
	CreatedAt     int64
	UpdatedAt     int64
	CompletedAt   int64
}

// TaskOutputMessage mirrors internal/thread.TaskOutputMessage.
type TaskOutputMessage struct {
	Role string
	Text string
}

// TaskOutput mirrors internal/thread.TaskOutput: a tail of a task's child
// session transcript, plus Total so a caller can tell a truncated tail
// from the whole thing.
type TaskOutput struct {
	Messages []TaskOutputMessage
	Total    int
}

// TaskManager is the subset of internal/thread.TaskManager's API the
// built-in agent tool's background mode and the task_* tools need.
type TaskManager interface {
	// Create starts a new task, as the "agent" tool's background mode
	// uses.
	Create(ctx context.Context, args TaskCreateArgs) (TaskInfo, error)
	// List returns every task in the workspace, for task_list.
	List(ctx context.Context) ([]TaskInfo, error)
	// Get resolves id to a task, for task_result (and task_cancel's
	// after-the-fact status report).
	Get(ctx context.Context, id string) (TaskInfo, error)
	// Cancel stops id's in-flight run, recording reason as its terminal
	// error, for task_cancel.
	Cancel(ctx context.Context, id, reason string) error
	// Send dispatches message into id's session, reactivating it first if
	// not live, for task_send.
	Send(ctx context.Context, id, message string) error
	// Output returns a tail of id's child session transcript (at most
	// limit messages; <= 0 means the implementation's own default), for
	// task_output.
	Output(ctx context.Context, id string, limit int) (TaskOutput, error)
}
