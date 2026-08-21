package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"strings"

	"charm.land/fantasy"
)

const TaskOutputToolName = "task_output"

//go:embed task_output.md.tpl
var taskOutputDescriptionTmpl []byte

var taskOutputDescriptionTpl = template.Must(
	template.New("taskOutputDescription").Parse(string(taskOutputDescriptionTmpl)),
)

type TaskOutputParams struct {
	ID    string `json:"id" description:"The task's ID"`
	Limit int    `json:"limit,omitempty" description:"Max number of recent messages to return (default 20, max 100)"`
}

// NewTaskOutputTool creates the task_output tool. See [NewTaskListTool]
// for the manager nil-safety note.
func NewTaskOutputTool(manager TaskManager) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		TaskOutputToolName,
		renderToolDescription(taskOutputDescriptionTpl),
		func(ctx context.Context, params TaskOutputParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ID == "" {
				return invalidParam("id"), nil
			}

			out, err := manager.Output(ctx, params.ID, params.Limit)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			if len(out.Messages) == 0 {
				return fantasy.WithResponseMetadata(fantasy.NewTextResponse("No output yet."), out), nil
			}

			var sb strings.Builder
			if out.Total > len(out.Messages) {
				fmt.Fprintf(&sb, "Showing last %d of %d messages.\n\n", len(out.Messages), out.Total)
			}
			for _, m := range out.Messages {
				fmt.Fprintf(&sb, "[%s] %s\n\n", m.Role, m.Text)
			}

			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(strings.TrimSpace(sb.String())), out), nil
		},
	)
}
