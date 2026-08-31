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
func NewTaskCancelTool(manager TaskManager, permissions permission.Requester) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(
		TaskCancelToolName,
		renderToolDescription(taskCancelDescriptionTpl),
		func(ctx context.Context, params TaskCancelParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ID == "" {
				return invalidParam("id"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, missingSessionID(TaskCancelToolName)
			}

			// Only the tasks this caller started, however many levels
			// down. A delegation reaching for its own id here - the
			// mistake that made this check exist - stops its own turn
			// dead, taking its report and its children with it. See
			// taskScope.
			scope, failed, err := scopeTasks(ctx, manager, TaskCancelToolName)
			if err != nil || failed != nil {
				return derefResponse(failed), err
			}
			if refusal, refused := scope.refuse(params.ID, "cancel"); refused {
				return refusal, nil
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
	), map[string]toolParameterSchema{"id": {minLength: intPtr(1)}})
}
