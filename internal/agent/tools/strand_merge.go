package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"

	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/permission"
)

const StrandMergeToolName = "strand_merge"

//go:embed strand_merge.md.tpl
var strandMergeDescriptionTmpl []byte

var strandMergeDescriptionTpl = template.Must(
	template.New("strandMergeDescription").Parse(string(strandMergeDescriptionTmpl)),
)

type StrandMergeParams struct {
	ID string `json:"id" description:"The strand's ID or name"`
}

type StrandMergePermissionParams struct {
	ID string `json:"id"`
}

// NewStrandMergeTool creates the strand_merge tool. See
// [NewStrandCreateTool] for the manager nil-safety note.
func NewStrandMergeTool(manager StrandManager, permissions permission.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		StrandMergeToolName,
		renderToolDescription(strandMergeDescriptionTpl),
		func(ctx context.Context, params StrandMergeParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ID == "" {
				return fantasy.NewTextErrorResponse("id is required"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for strand_merge")
			}

			resp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				ToolCallID:  call.ID,
				ToolName:    StrandMergeToolName,
				Action:      "merge",
				Description: fmt.Sprintf("Merge strand %q", params.ID),
				Params:      StrandMergePermissionParams{ID: params.ID},
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if denied {
				return resp, nil
			}

			if err := manager.Merge(ctx, params.ID); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			st, err := manager.Get(ctx, params.ID)
			if err != nil {
				return fantasy.NewTextResponse("Merge attempt finished."), nil
			}

			switch st.Status {
			case "merged":
				return fantasy.NewTextResponse(fmt.Sprintf("Strand %q merged.", params.ID)), nil
			case "conflict":
				return fantasy.NewTextResponse(fmt.Sprintf("Strand %q has merge conflicts: %s", params.ID, st.Error)), nil
			default:
				return fantasy.NewTextResponse(fmt.Sprintf("Strand %q status: %s (%s)", params.ID, st.Status, st.Error)), nil
			}
		},
	)
}
