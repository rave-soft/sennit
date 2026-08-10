package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"

	"charm.land/fantasy"
)

const StrandStatusToolName = "strand_status"

//go:embed strand_status.md.tpl
var strandStatusDescriptionTmpl []byte

var strandStatusDescriptionTpl = template.Must(
	template.New("strandStatusDescription").Parse(string(strandStatusDescriptionTmpl)),
)

type StrandStatusParams struct {
	ID string `json:"id" description:"The strand's ID or name"`
}

// NewStrandStatusTool creates the strand_status tool. See
// [NewStrandCreateTool] for the manager nil-safety note.
func NewStrandStatusTool(manager StrandManager) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		StrandStatusToolName,
		renderToolDescription(strandStatusDescriptionTpl),
		func(ctx context.Context, params StrandStatusParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
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
