package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"

	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/permission"
)

const ThreadCreateToolName = "thread_create"

//go:embed thread_create.md.tpl
var threadCreateDescriptionTmpl []byte

var threadCreateDescriptionTpl = template.Must(
	template.New("threadCreateDescription").Parse(string(threadCreateDescriptionTmpl)),
)

type ThreadCreateParams struct {
	Name        string `json:"name" description:"A short slug (lowercase letters, digits, hyphens) used as the thread's branch name and worktree directory"`
	Goal        string `json:"goal" description:"The task to hand to the thread's agent"`
	BaseBranch  string `json:"base_branch,omitempty" description:"Branch to fork from (defaults to the repository's current branch)"`
	MergePolicy string `json:"merge_policy,omitempty" description:"auto (default) or manual"`
}

type ThreadCreatePermissionParams struct {
	Name       string `json:"name"`
	Goal       string `json:"goal"`
	BaseBranch string `json:"base_branch,omitempty"`
}

type ThreadResponseMetadata struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Branch       string `json:"branch"`
	WorktreePath string `json:"worktree_path"`
	Status       string `json:"status"`
}

// NewThreadCreateTool creates the thread_create tool. manager is nil-safe
// only in that the coordinator omits this tool entirely when manager is
// nil (see coordinator.buildTools); it is never constructed with a nil
// manager.
func NewThreadCreateTool(manager ThreadManager, permissions permission.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ThreadCreateToolName,
		renderToolDescription(threadCreateDescriptionTpl),
		func(ctx context.Context, params ThreadCreateParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Name == "" {
				return fantasy.NewTextErrorResponse("name is required"), nil
			}
			if params.Goal == "" {
				return fantasy.NewTextErrorResponse("goal is required"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for thread_create")
			}

			resp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				ToolCallID:  call.ID,
				ToolName:    ThreadCreateToolName,
				Action:      "create",
				Description: fmt.Sprintf("Create thread %q", params.Name),
				Params: ThreadCreatePermissionParams{
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

			st, err := manager.Create(ctx, ThreadCreateArgs(params))
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			text := fmt.Sprintf(
				"Created thread %q (id=%s, status=%s)\nbranch: %s\nworktree: %s",
				st.Name, st.ID, st.Status, st.Branch, st.WorktreePath,
			)
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), ThreadResponseMetadata{
				ID:           st.ID,
				Name:         st.Name,
				Branch:       st.Branch,
				WorktreePath: st.WorktreePath,
				Status:       st.Status,
			}), nil
		},
	)
}
