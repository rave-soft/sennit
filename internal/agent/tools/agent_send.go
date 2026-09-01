package tools

import (
	"context"
	_ "embed"
	"html/template"

	"charm.land/fantasy"
)

const AgentSendToolName = "agent_send"

//go:embed agent_send.md.tpl
var agentSendDescriptionTmpl []byte

var agentSendDescriptionTpl = template.Must(
	template.New("agentSendDescription").Parse(string(agentSendDescriptionTmpl)),
)

type AgentSendParams struct {
	ID      string `json:"id" description:"The delegation's ID, or a thread's name"`
	Message string `json:"message" description:"The instruction to send"`
}

// NewAgentSendTool creates the agent_send tool. See [NewAgentListTool]
// for the manager nil-safety note.
func NewAgentSendTool(tasks TaskManager, threads ThreadManager) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(
		AgentSendToolName,
		renderToolDescription(agentSendDescriptionTpl),
		func(ctx context.Context, params AgentSendParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ID == "" {
				return invalidParam("id"), nil
			}
			if params.Message == "" {
				return invalidParam("message"), nil
			}

			// Scoped exactly like agent_cancel, against exactly the same
			// mistake: a delegation that sends to its own id feeds its
			// own session, and one that sends upward drives the turn
			// that is waiting on it. See taskScope.
			view, failed, err := resolveDelegations(ctx, tasks, threads, AgentSendToolName)
			if err != nil || failed != nil {
				return derefResponse(failed), err
			}
			ref, refusal := view.lookup(ctx, tasks, params.ID, "send to")
			if refusal != nil {
				return *refusal, nil
			}

			var outcome SendOutcome
			if ref.Kind == KindThread {
				outcome, err = threads.Send(ctx, params.ID, params.Message)
			} else {
				outcome, err = tasks.Send(ctx, ref.Task.ID, params.Message)
			}
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(outcome.Describe(string(ref.Kind), params.ID)), nil
		},
	), map[string]toolParameterSchema{"id": {minLength: intPtr(1)}, "message": {minLength: intPtr(1)}})
}
