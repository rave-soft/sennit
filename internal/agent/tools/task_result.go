package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"

	"charm.land/fantasy"
)

const TaskResultToolName = "task_result"

//go:embed task_result.md.tpl
var taskResultDescriptionTmpl []byte

var taskResultDescriptionTpl = template.Must(
	template.New("taskResultDescription").Parse(string(taskResultDescriptionTmpl)),
)

type TaskResultParams struct {
	ID string `json:"id" description:"The task's ID"`
}

// NewTaskResultTool creates the task_result tool. See [NewTaskListTool]
// for the manager nil-safety note.
func NewTaskResultTool(manager TaskManager) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(
		TaskResultToolName,
		renderToolDescription(taskResultDescriptionTpl),
		func(ctx context.Context, params TaskResultParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ID == "" {
				return invalidParam("id"), nil
			}

			ti, err := manager.Get(ctx, params.ID)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			var text string
			switch ti.Status {
			case "completed":
				text = fmt.Sprintf("Task %s finished.\n\n%s", ti.ID, ti.ResultSummary)
			case "failed", "interrupted", "cancelled":
				text = fmt.Sprintf("Task %s did not complete (status=%s): %s", ti.ID, ti.Status, ti.Error)
			default:
				text = fmt.Sprintf("Task %s is still %s; no result yet.", ti.ID, ti.Status)
			}

			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), ti), nil
		},
	), map[string]toolParameterSchema{"id": {minLength: intPtr(1)}})
}
