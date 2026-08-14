package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"

	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/permission"
)

const ThreadMergeToolName = "thread_merge"

//go:embed thread_merge.md.tpl
var threadMergeDescriptionTmpl []byte

var threadMergeDescriptionTpl = template.Must(
	template.New("threadMergeDescription").Parse(string(threadMergeDescriptionTmpl)),
)

type ThreadMergeParams struct {
	ID string `json:"id" description:"The thread's ID or name"`
}

type ThreadMergePermissionParams struct {
	ID string `json:"id"`
}

// NewThreadMergeTool creates the thread_merge tool. See
// [NewThreadCreateTool] for the manager nil-safety note.
func NewThreadMergeTool(manager ThreadManager, permissions permission.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ThreadMergeToolName,
		renderToolDescription(threadMergeDescriptionTpl),
		func(ctx context.Context, params ThreadMergeParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ID == "" {
				return fantasy.NewTextErrorResponse("id is required"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for thread_merge")
			}

			resp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				ToolCallID:  call.ID,
				ToolName:    ThreadMergeToolName,
				Action:      "merge",
				Description: fmt.Sprintf("Merge thread %q", params.ID),
				Params:      ThreadMergePermissionParams(params),
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if denied {
				return resp, nil
			}

			st, err := manager.Merge(ctx, params.ID)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			switch st.Status {
			case "merged":
				return fantasy.NewTextResponse(fmt.Sprintf(
					"Thread %q merged into %s; its worktree and branch have been removed.", params.ID, st.BaseBranch)), nil
			case "conflict":
				return fantasy.NewTextResponse(fmt.Sprintf("Thread %q has merge conflicts: %s", params.ID, st.Error)), nil
			default:
				return fantasy.NewTextResponse(fmt.Sprintf("Thread %q status: %s (%s)", params.ID, st.Status, st.Error)), nil
			}
		},
	)
}
