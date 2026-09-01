package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"strings"

	"charm.land/fantasy"
)

const AgentListToolName = "agent_list"

//go:embed agent_list.md.tpl
var agentListDescriptionTmpl []byte

var agentListDescriptionTpl = template.Must(
	template.New("agentListDescription").Parse(string(agentListDescriptionTmpl)),
)

type AgentListParams struct{}

// AgentListResponseMetadata carries both kinds separately rather than one
// merged list: the UI and any later reader still need to tell a task from
// a thread, and the two records genuinely differ (a thread has a branch
// and a worktree, a task has neither).
type AgentListResponseMetadata struct {
	Tasks   []TaskInfo   `json:"tasks"`
	Threads []ThreadInfo `json:"threads"`
}

// NewAgentListTool creates the agent_list tool. tasks is never nil (the
// registry omits every agent_* tool when it is); threads is nil in a
// workspace with no thread manager, and then only tasks are listed.
func NewAgentListTool(tasks TaskManager, threads ThreadManager) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		AgentListToolName,
		renderToolDescription(agentListDescriptionTpl),
		func(ctx context.Context, params AgentListParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			view, failed, err := resolveDelegations(ctx, tasks, threads, AgentListToolName)
			if err != nil || failed != nil {
				return derefResponse(failed), err
			}
			// The caller's own subtree, not the workspace: see taskScope.
			taskRows := view.scope.subtree()

			var threadRows []ThreadInfo
			if threads != nil {
				threadRows, err = threads.List(ctx)
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
			}

			if len(taskRows) == 0 && len(threadRows) == 0 {
				return fantasy.WithResponseMetadata(
					fantasy.NewTextResponse("No delegations."),
					AgentListResponseMetadata{},
				), nil
			}

			var sb strings.Builder
			for _, ti := range taskRows {
				fmt.Fprintf(&sb, "%s\t%s\t%s\t%s\n", ti.ID, KindTask, ti.Status, firstLine(ti.Goal))
			}
			for _, st := range threadRows {
				summary := st.ResultSummary
				if summary == "" {
					summary = st.Error
				}
				if summary == "" {
					summary = st.Goal
				}
				fmt.Fprintf(&sb, "%s\t%s\t%s\t%s\t%s\n", st.ID, KindThread, st.Status, st.Name, firstLine(summary))
			}

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(sb.String()),
				AgentListResponseMetadata{Tasks: taskRows, Threads: threadRows},
			), nil
		},
	)
}

// firstLine returns s up to its first newline, for one-line summaries.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
