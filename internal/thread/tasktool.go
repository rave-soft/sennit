package thread

import (
	"context"

	"github.com/rave-soft/braid/internal/agent/tools"
)

// agentToolTaskManager adapts a [TaskManager] to [tools.TaskManager], the
// interface the built-in agent tool's background mode is built against.
// Exists for the same import-cycle reason [agentToolManager] does — see
// its doc comment.
type agentToolTaskManager struct {
	t *TaskManager
}

// AsAgentToolTaskManager returns t adapted to [tools.TaskManager], for
// wiring into agent.CoordinatorOptions (or app.SetTasks, which forwards
// to it).
func AsAgentToolTaskManager(t *TaskManager) tools.TaskManager {
	return &agentToolTaskManager{t: t}
}

func (a *agentToolTaskManager) Create(ctx context.Context, args tools.TaskCreateArgs) (tools.TaskInfo, error) {
	st, err := a.t.Create(ctx, TaskCreateArgs{
		Goal:            args.Goal,
		ParentSessionID: args.ParentSessionID,
	})
	if err != nil {
		return tools.TaskInfo{}, err
	}
	return tools.TaskInfo{
		ID:        st.ID,
		SessionID: st.SessionID,
		Status:    string(st.Status),
	}, nil
}
