package threadspawn

import (
	"context"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/thread"
)

// agentToolTaskManager adapts a *thread.TaskManager to tools.TaskManager,
// the interface the built-in agent tool's background mode and the task_*
// tools are built against. Exists for the same import-cycle reason
// agentToolManager does — see its doc comment.
type agentToolTaskManager struct {
	t *thread.TaskManager
}

// AsAgentToolTaskManager returns t adapted to tools.TaskManager, for
// wiring into agent.CoordinatorOptions (or app.SetTasks, which forwards
// to it).
func AsAgentToolTaskManager(t *thread.TaskManager) tools.TaskManager {
	return &agentToolTaskManager{t: t}
}

func (a *agentToolTaskManager) Create(ctx context.Context, args tools.TaskCreateArgs) (tools.TaskInfo, error) {
	st, err := a.t.Create(ctx, thread.TaskCreateArgs{
		Goal:            args.Goal,
		ParentSessionID: args.ParentSessionID,
		Depth:           args.Depth,
		SessionTitle:    args.SessionTitle,
		AgentID:         args.AgentID,
		SessionID:       args.SessionID,
		Factory:         adaptTaskFactory(args.Factory),
	})
	if err != nil {
		return tools.TaskInfo{}, err
	}
	return toTaskInfo(st), nil
}

func adaptTaskFactory(factory tools.TaskRunFactory) thread.TaskRunFactory {
	if factory == nil {
		return nil
	}
	return func(ctx context.Context, childSessionID string) (func(context.Context) (thread.TaskRunResult, error), func(), error) {
		run, cleanup, err := factory(ctx, childSessionID)
		if run == nil {
			return nil, cleanup, err
		}
		return func(ctx context.Context) (thread.TaskRunResult, error) {
			result, err := run(ctx)
			return thread.TaskRunResult{Text: result.Text}, err
		}, cleanup, err
	}
}

func (a *agentToolTaskManager) List(ctx context.Context) ([]tools.TaskInfo, error) {
	tasks, err := a.t.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tools.TaskInfo, len(tasks))
	for i, st := range tasks {
		out[i] = toTaskInfo(st)
	}
	return out, nil
}

func (a *agentToolTaskManager) Get(ctx context.Context, id string) (tools.TaskInfo, error) {
	st, err := a.t.Get(ctx, id)
	if err != nil {
		return tools.TaskInfo{}, err
	}
	return toTaskInfo(st), nil
}

func (a *agentToolTaskManager) Cancel(ctx context.Context, id, reason string) error {
	return a.t.Cancel(ctx, id, reason)
}

func (a *agentToolTaskManager) Send(ctx context.Context, id, message string) (tools.SendOutcome, error) {
	disp, err := a.t.Send(ctx, id, message)
	if err != nil {
		return tools.SendOutcome{}, err
	}
	return toToolSendOutcome(disp), nil
}

func (a *agentToolTaskManager) Output(ctx context.Context, id string, limit int) (tools.TaskOutput, error) {
	out, err := a.t.Output(ctx, id, limit)
	if err != nil {
		return tools.TaskOutput{}, err
	}
	messages := make([]tools.TaskOutputMessage, len(out.Messages))
	for i, m := range out.Messages {
		messages[i] = tools.TaskOutputMessage{Role: m.Role, Text: m.Text}
	}
	return tools.TaskOutput{Messages: messages, Total: out.Total}, nil
}

func toTaskInfo(st thread.Thread) tools.TaskInfo {
	return tools.TaskInfo{
		ID:            st.ID,
		Goal:          st.Goal,
		SessionID:     st.SessionID,
		Status:        string(st.Status),
		ResultSummary: st.ResultSummary,
		Error:         st.Error,
		CreatedAt:     st.CreatedAt,
		UpdatedAt:     st.UpdatedAt,
		CompletedAt:   st.CompletedAt,
	}
}
