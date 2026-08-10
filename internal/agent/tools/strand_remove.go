package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"

	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/permission"
)

const StrandRemoveToolName = "strand_remove"

//go:embed strand_remove.md.tpl
var strandRemoveDescriptionTmpl []byte

var strandRemoveDescriptionTpl = template.Must(
	template.New("strandRemoveDescription").Parse(string(strandRemoveDescriptionTmpl)),
)

type StrandRemoveParams struct {
	ID           string `json:"id" description:"The strand's ID or name"`
	Force        bool   `json:"force,omitempty" description:"Remove even if active or dirty/unmerged"`
	DeleteBranch bool   `json:"delete_branch,omitempty" description:"Also delete the strand's git branch"`
}

type StrandRemovePermissionParams struct {
	ID           string `json:"id"`
	Force        bool   `json:"force,omitempty"`
	DeleteBranch bool   `json:"delete_branch,omitempty"`
}

// NewStrandRemoveTool creates the strand_remove tool. See
// [NewStrandCreateTool] for the manager nil-safety note.
func NewStrandRemoveTool(manager StrandManager, permissions permission.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		StrandRemoveToolName,
		renderToolDescription(strandRemoveDescriptionTpl),
		func(ctx context.Context, params StrandRemoveParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ID == "" {
				return fantasy.NewTextErrorResponse("id is required"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for strand_remove")
			}

			resp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				ToolCallID:  call.ID,
				ToolName:    StrandRemoveToolName,
				Action:      "remove",
				Description: fmt.Sprintf("Remove strand %q", params.ID),
				Params:      StrandRemovePermissionParams(params),
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

			return fantasy.NewTextResponse(fmt.Sprintf("Removed strand %q", params.ID)), nil
		},
	)
}
