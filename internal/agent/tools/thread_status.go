package tools

import (
	"context"
	_ "embed"
	"errors"
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
				return invalidParam("id"), nil
			}

			st, err := manager.Get(ctx, params.ID)
			if errors.Is(err, ErrThreadNotFound) {
				// Say what the absence most likely means. A thread is
				// deleted once its branch lands, so "not found" is the
				// normal answer for one that finished — reporting only
				// the miss invites the model to conclude the thread was
				// lost and start it over.
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"no thread %q. A thread is removed once it merges — if it existed, "+
						"it most likely landed and was cleared away; its merge is recorded "+
						"in the session history. Use thread_list to see what is still running.",
					params.ID)), nil
			}
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
