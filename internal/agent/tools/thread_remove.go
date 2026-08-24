package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/permission"
)

const ThreadRemoveToolName = "thread_remove"

//go:embed thread_remove.md.tpl
var threadRemoveDescriptionTmpl []byte

var threadRemoveDescriptionTpl = template.Must(
	template.New("threadRemoveDescription").Parse(string(threadRemoveDescriptionTmpl)),
)

type ThreadRemoveParams struct {
	ID           string `json:"id" description:"The thread's ID or name"`
	Force        bool   `json:"force,omitempty" description:"Remove even if active or dirty/unmerged"`
	DeleteBranch bool   `json:"delete_branch,omitempty" description:"Also delete the thread's git branch"`
}

type ThreadRemovePermissionParams struct {
	ID           string `json:"id"`
	Force        bool   `json:"force,omitempty"`
	DeleteBranch bool   `json:"delete_branch,omitempty"`
}

// NewThreadRemoveTool creates the thread_remove tool. See
// [NewThreadCreateTool] for the manager nil-safety note.
func NewThreadRemoveTool(manager ThreadManager, permissions permission.Service) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(
		ThreadRemoveToolName,
		renderToolDescription(threadRemoveDescriptionTpl),
		func(ctx context.Context, params ThreadRemoveParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ID == "" {
				return invalidParam("id"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, missingSessionID("thread_remove")
			}

			resp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				ToolCallID:  call.ID,
				ToolName:    ThreadRemoveToolName,
				Action:      "remove",
				Description: fmt.Sprintf("Remove thread %q", params.ID),
				Params:      ThreadRemovePermissionParams(params),
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if denied {
				return resp, nil
			}

			if err := manager.Remove(ctx, params.ID, params.Force, params.DeleteBranch); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			return fantasy.NewTextResponse(fmt.Sprintf("Removed thread %q", params.ID)), nil
		},
	), map[string]toolParameterSchema{"id": {minLength: intPtr(1)}})
}
