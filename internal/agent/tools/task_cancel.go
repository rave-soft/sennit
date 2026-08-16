package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/permission"
)

const TaskCancelToolName = "task_cancel"

//go:embed task_cancel.md.tpl
var taskCancelDescriptionTmpl []byte

var taskCancelDescriptionTpl = template.Must(
	template.New("taskCancelDescription").Parse(string(taskCancelDescriptionTmpl)),
)

type TaskCancelParams struct {
	ID     string `json:"id" description:"The task's ID"`
	Reason string `json:"reason,omitempty" description:"Why the task is being cancelled"`
}

type TaskCancelPermissionParams struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// NewTaskCancelTool creates the task_cancel tool. See [NewTaskListTool]
// for the manager nil-safety note.
func NewTaskCancelTool(manager TaskManager, permissions permission.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		TaskCancelToolName,
		renderToolDescription(taskCancelDescriptionTpl),
		func(ctx context.Context, params TaskCancelParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ID == "" {
				return fantasy.NewTextErrorResponse("id is required"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for task_cancel")
			}

			resp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				ToolCallID:  call.ID,
				ToolName:    TaskCancelToolName,
				Action:      "cancel",
				Description: fmt.Sprintf("Cancel task %q", params.ID),
				Params:      TaskCancelPermissionParams(params),
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if denied {
				return resp, nil
			}

			if err := manager.Cancel(ctx, params.ID, params.Reason); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			ti, err := manager.Get(ctx, params.ID)
			if err != nil {
				return fantasy.NewTextResponse("Cancel attempt finished."), nil
			}

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(fmt.Sprintf("Task %s status: %s", ti.ID, ti.Status)),
				ti,
			), nil
		},
	)
}
