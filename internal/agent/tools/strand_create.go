package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"

	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/permission"
)

const StrandCreateToolName = "strand_create"

//go:embed strand_create.md.tpl
var strandCreateDescriptionTmpl []byte

var strandCreateDescriptionTpl = template.Must(
	template.New("strandCreateDescription").Parse(string(strandCreateDescriptionTmpl)),
)

type StrandCreateParams struct {
	Name        string `json:"name" description:"A short slug (lowercase letters, digits, hyphens) used as the strand's branch name and worktree directory"`
	Goal        string `json:"goal" description:"The task to hand to the strand's agent"`
	BaseBranch  string `json:"base_branch,omitempty" description:"Branch to fork from (defaults to the repository's current branch)"`
	MergePolicy string `json:"merge_policy,omitempty" description:"auto (default) or manual"`
}

type StrandCreatePermissionParams struct {
	Name       string `json:"name"`
	Goal       string `json:"goal"`
	BaseBranch string `json:"base_branch,omitempty"`
}

type StrandResponseMetadata struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Branch       string `json:"branch"`
	WorktreePath string `json:"worktree_path"`
	Status       string `json:"status"`
}

// NewStrandCreateTool creates the strand_create tool. manager is nil-safe
// only in that the coordinator omits this tool entirely when manager is
// nil (see coordinator.buildTools); it is never constructed with a nil
// manager.
func NewStrandCreateTool(manager StrandManager, permissions permission.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		StrandCreateToolName,
		renderToolDescription(strandCreateDescriptionTpl),
		func(ctx context.Context, params StrandCreateParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Name == "" {
				return fantasy.NewTextErrorResponse("name is required"), nil
			}
			if params.Goal == "" {
				return fantasy.NewTextErrorResponse("goal is required"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for strand_create")
			}

			resp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				ToolCallID:  call.ID,
				ToolName:    StrandCreateToolName,
				Action:      "create",
				Description: fmt.Sprintf("Create strand %q", params.Name),
				Params: StrandCreatePermissionParams{
					Name:       params.Name,
					Goal:       params.Goal,
					BaseBranch: params.BaseBranch,
				},
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if denied {
				return resp, nil
			}

			st, err := manager.Create(ctx, StrandCreateArgs{
				Name:        params.Name,
				Goal:        params.Goal,
				BaseBranch:  params.BaseBranch,
				MergePolicy: params.MergePolicy,
			})
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			text := fmt.Sprintf(
				"Created strand %q (id=%s, status=%s)\nbranch: %s\nworktree: %s",
				st.Name, st.ID, st.Status, st.Branch, st.WorktreePath,
			)
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), StrandResponseMetadata{
				ID:           st.ID,
				Name:         st.Name,
				Branch:       st.Branch,
				WorktreePath: st.WorktreePath,
				Status:       st.Status,
			}), nil
		},
	)
}
