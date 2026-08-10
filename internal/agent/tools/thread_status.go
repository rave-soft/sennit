package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"

	"charm.land/fantasy"
)

const ThreadStatusToolName = "thread_status"

//go:embed thread_status.md.tpl
var threadStatusDescriptionTmpl []byte

var threadStatusDescriptionTpl = template.Must(
	template.New("threadStatusDescription").Parse(string(threadStatusDescriptionTmpl)),
)

type ThreadStatusParams struct {
	ID string `json:"id" description:"The thread's ID or name"`
}

// NewThreadStatusTool creates the thread_status tool. See
// [NewThreadCreateTool] for the manager nil-safety note.
func NewThreadStatusTool(manager ThreadManager) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ThreadStatusToolName,
		renderToolDescription(threadStatusDescriptionTpl),
		func(ctx context.Context, params ThreadStatusParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ID == "" {
				return fantasy.NewTextErrorResponse("id is required"), nil
			}

			st, err := manager.Get(ctx, params.ID)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			text := fmt.Sprintf(
				"id: %s\nname: %s\nstatus: %s\nbranch: %s\nbase_branch: %s\nworktree: %s\nmerge_policy: %s\ngoal: %s\nresult_summary: %s\nerror: %s",
				st.ID, st.Name, st.Status, st.Branch, st.BaseBranch, st.WorktreePath, st.MergePolicy, st.Goal, st.ResultSummary, st.Error,
			)
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), st), nil
		},
	)
}
