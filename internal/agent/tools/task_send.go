package tools

import (
	"context"
	_ "embed"
	"html/template"

	"charm.land/fantasy"
)

const TaskSendToolName = "task_send"

//go:embed task_send.md.tpl
var taskSendDescriptionTmpl []byte

var taskSendDescriptionTpl = template.Must(
	template.New("taskSendDescription").Parse(string(taskSendDescriptionTmpl)),
)

type TaskSendParams struct {
	ID      string `json:"id" description:"The task's ID"`
	Message string `json:"message" description:"The instruction to send to the task"`
}

// NewTaskSendTool creates the task_send tool. See [NewTaskListTool] for
// the manager nil-safety note.
func NewTaskSendTool(manager TaskManager) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(
		TaskSendToolName,
		renderToolDescription(taskSendDescriptionTpl),
		func(ctx context.Context, params TaskSendParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ID == "" {
				return invalidParam("id"), nil
			}
			if params.Message == "" {
				return invalidParam("message"), nil
			}

			// Scoped exactly like task_cancel, against exactly the same
			// mistake: a delegation that sends to its own id feeds its
			// own session, and one that sends upward drives the turn
			// that is waiting on it. See taskScope.
			scope, failed, err := scopeTasks(ctx, manager, TaskSendToolName)
			if err != nil || failed != nil {
				return derefResponse(failed), err
			}
			if refusal, refused := scope.refuse(params.ID, "send to"); refused {
				return refusal, nil
			}

			outcome, err := manager.Send(ctx, params.ID, params.Message)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			return fantasy.NewTextResponse(outcome.Describe("task", params.ID)), nil
		},
	), map[string]toolParameterSchema{"id": {minLength: intPtr(1)}, "message": {minLength: intPtr(1)}})
}
