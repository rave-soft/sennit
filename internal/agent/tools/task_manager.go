package tools

import "context"

// TaskCreateArgs mirrors internal/thread.TaskCreateArgs. It is declared
// here, rather than imported, for the same reason ThreadCreateArgs is —
// see that type's doc comment.
type TaskCreateArgs struct {
	Goal            string
	ParentSessionID string
	// SessionTitle and AgentID preserve the child session identity of a
	// specialized delegation. Empty values retain the built-in task defaults.
	SessionTitle string
	AgentID      string
	// Factory performs all potentially blocking preparation after Create has
	// returned its acknowledgement. Cleanup is owned by the task lifecycle and
	// is called exactly once, including when preparation fails.
	Factory TaskRunFactory
	// Depth is the cascade depth of the turn creating this task (0 for a
	// real user turn; see internal/agent's DepthContextKey/completion
	// continuation depth). The task inherits it as-is — a continuation
	// created from this task's completion runs one level deeper.
	Depth int
}

// TaskRunResult is the terminal output of a specialized delegation.
type TaskRunResult struct {
	Text string
}

// TaskRunFactory prepares a specialized delegation. Cleanup may be non-nil
// together with an error when preparation acquired resources before failing.
type TaskRunFactory func(ctx context.Context, childSessionID string) (run func(context.Context) (TaskRunResult, error), cleanup func(), err error)

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
// asynchronous delegation launcher and the task_* tools need.
type TaskManager interface {
	// Create starts a new asynchronous delegation.
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
	// not live, for task_send. The [SendOutcome] reports whether the task's
	// agent reads the message now or only after the turn it is already
	// running.
	Send(ctx context.Context, id, message string) (SendOutcome, error)
	// Output returns a tail of id's child session transcript (at most
	// limit messages; <= 0 means the implementation's own default), for
	// task_output.
	Output(ctx context.Context, id string, limit int) (TaskOutput, error)
}
