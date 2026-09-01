package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"strings"

	"charm.land/fantasy"
)

const AgentResultToolName = "agent_result"

//go:embed agent_result.md.tpl
var agentResultDescriptionTmpl []byte

var agentResultDescriptionTpl = template.Must(
	template.New("agentResultDescription").Parse(string(agentResultDescriptionTmpl)),
)

type AgentResultParams struct {
	ID string `json:"id" description:"The delegation's ID, or a thread's name"`
}

// NewAgentResultTool creates the agent_result tool. See
// [NewAgentListTool] for the manager nil-safety note.
func NewAgentResultTool(tasks TaskManager, threads ThreadManager) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(
		AgentResultToolName,
		renderToolDescription(agentResultDescriptionTpl),
		func(ctx context.Context, params AgentResultParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ID == "" {
				return invalidParam("id"), nil
			}
			view, failed, err := resolveDelegations(ctx, tasks, threads, AgentResultToolName)
			if err != nil || failed != nil {
				return derefResponse(failed), err
			}
			ref, refusal := view.lookup(ctx, tasks, params.ID, "read")
			if refusal != nil {
				return *refusal, nil
			}
			if ref.Kind == KindThread {
				return fantasy.WithResponseMetadata(
					fantasy.NewTextResponse(describeThread(ref.Thread)), ref.Thread,
				), nil
			}
			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(describeTaskResult(ref.Task)), ref.Task,
			), nil
		},
	), map[string]toolParameterSchema{"id": {minLength: intPtr(1)}})
}

// describeTaskResult renders a task's outcome, branching on whether it has
// one yet: a finished task answers with what it reported, and a running
// one answers with the fact that there is nothing to read yet, since a
// caller that cannot tell those apart tends to treat an empty summary as
// an empty result.
func describeTaskResult(ti TaskInfo) string {
	switch ti.Status {
	case "completed":
		return fmt.Sprintf("Task %s finished.\n\n%s", ti.ID, ti.ResultSummary)
	case "failed", "interrupted", "cancelled":
		return fmt.Sprintf("Task %s did not complete (status=%s): %s", ti.ID, ti.Status, ti.Error)
	default:
		return fmt.Sprintf("Task %s is still %s; no result yet.", ti.ID, ti.Status)
	}
}

// describeThread renders a thread's state, including the worktree facts a
// task has no equivalent of - the branch and path are how a person or a
// later merge finds the work.
func describeThread(st ThreadInfo) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Thread %s (%s) status: %s\n", st.ID, st.Name, st.Status)
	if st.Branch != "" {
		fmt.Fprintf(&sb, "Branch: %s (from %s)\n", st.Branch, st.BaseBranch)
	}
	if st.WorktreePath != "" {
		fmt.Fprintf(&sb, "Worktree: %s\n", st.WorktreePath)
	}
	if st.ResultSummary != "" {
		fmt.Fprintf(&sb, "\n%s\n", st.ResultSummary)
	}
	if st.Error != "" {
		fmt.Fprintf(&sb, "\nError: %s\n", st.Error)
	}
	return strings.TrimRight(sb.String(), "\n")
}
