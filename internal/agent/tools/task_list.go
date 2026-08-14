package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"strings"

	"charm.land/fantasy"
)

const TaskListToolName = "task_list"

//go:embed task_list.md.tpl
var taskListDescriptionTmpl []byte

var taskListDescriptionTpl = template.Must(
	template.New("taskListDescription").Parse(string(taskListDescriptionTmpl)),
)

type TaskListParams struct{}

type TaskListResponseMetadata struct {
	Tasks []TaskInfo `json:"tasks"`
}

// NewTaskListTool creates the task_list tool. manager is nil-safe only in
// that the coordinator omits this tool entirely when manager is nil (see
// coordinator.buildTools); it is never constructed with a nil manager.
func NewTaskListTool(manager TaskManager) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		TaskListToolName,
		renderToolDescription(taskListDescriptionTpl),
		func(ctx context.Context, params TaskListParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			tasks, err := manager.List(ctx)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			if len(tasks) == 0 {
				return fantasy.WithResponseMetadata(
					fantasy.NewTextResponse("No background tasks."),
					TaskListResponseMetadata{},
				), nil
			}

			var sb strings.Builder
			for _, ti := range tasks {
				fmt.Fprintf(&sb, "%s\t%s\t%s\n", ti.ID, ti.Status, firstLine(ti.Goal))
			}

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(sb.String()),
				TaskListResponseMetadata{Tasks: tasks},
			), nil
		},
	)
}
