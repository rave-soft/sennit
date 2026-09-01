package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/permission"
)

const AgentCancelToolName = "agent_cancel"

//go:embed agent_cancel.md.tpl
var agentCancelDescriptionTmpl []byte

var agentCancelDescriptionTpl = template.Must(
	template.New("agentCancelDescription").Parse(string(agentCancelDescriptionTmpl)),
)

type AgentCancelParams struct {
	ID     string `json:"id" description:"The delegation's ID, or a thread's name"`
	Reason string `json:"reason,omitempty" description:"Why the delegation is being cancelled"`
}

type AgentCancelPermissionParams struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// NewAgentCancelTool creates the agent_cancel tool. See
// [NewAgentListTool] for the manager nil-safety note.
func NewAgentCancelTool(tasks TaskManager, threads ThreadManager, permissions permission.Requester) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(
		AgentCancelToolName,
		renderToolDescription(agentCancelDescriptionTpl),
		func(ctx context.Context, params AgentCancelParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ID == "" {
				return invalidParam("id"), nil
			}
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, missingSessionID(AgentCancelToolName)
			}

			// Only the delegations this caller may act on. A delegation
			// reaching for its own id here - the mistake that made this
			// check exist - stops its own turn dead, taking its report
			// and its children with it. See taskScope.
			view, failed, err := resolveDelegations(ctx, tasks, threads, AgentCancelToolName)
			if err != nil || failed != nil {
				return derefResponse(failed), err
			}
			ref, refusal := view.lookup(ctx, tasks, params.ID, "cancel")
			if refusal != nil {
				return *refusal, nil
			}

			resp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				ToolCallID:  call.ID,
				ToolName:    AgentCancelToolName,
				Action:      "cancel",
				Description: fmt.Sprintf("Cancel %s %q", ref.Kind, params.ID),
				Params:      AgentCancelPermissionParams(params),
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if denied {
				return resp, nil
			}

			if ref.Kind == KindThread {
				if err := threads.Cancel(ctx, params.ID, params.Reason); err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				st, err := threads.Get(ctx, params.ID)
				if err != nil {
					return fantasy.NewTextResponse("Cancel attempt finished."), nil
				}
				return fantasy.WithResponseMetadata(
					fantasy.NewTextResponse(fmt.Sprintf("Thread %s status: %s", st.ID, st.Status)), st,
				), nil
			}

			if err := tasks.Cancel(ctx, ref.Task.ID, params.Reason); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			ti, err := tasks.Get(ctx, ref.Task.ID)
			if err != nil {
				return fantasy.NewTextResponse("Cancel attempt finished."), nil
			}
			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(fmt.Sprintf("Task %s status: %s", ti.ID, ti.Status)), ti,
			), nil
		},
	), map[string]toolParameterSchema{"id": {minLength: intPtr(1)}})
}
