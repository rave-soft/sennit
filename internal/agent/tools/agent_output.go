package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"strings"

	"charm.land/fantasy"
)

const AgentOutputToolName = "agent_output"

//go:embed agent_output.md.tpl
var agentOutputDescriptionTmpl []byte

var agentOutputDescriptionTpl = template.Must(
	template.New("agentOutputDescription").Parse(string(agentOutputDescriptionTmpl)),
)

type AgentOutputParams struct {
	ID    string `json:"id" description:"The task's ID"`
	Limit int    `json:"limit,omitempty" description:"Max number of recent messages to return (default 20, max 100)"`
}

// NewAgentOutputTool creates the agent_output tool. See
// [NewAgentListTool] for the manager nil-safety note.
func NewAgentOutputTool(tasks TaskManager, threads ThreadManager) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(
		AgentOutputToolName,
		renderToolDescription(agentOutputDescriptionTpl),
		func(ctx context.Context, params AgentOutputParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ID == "" {
				return invalidParam("id"), nil
			}
			if params.Limit < 0 || params.Limit > 100 {
				return fantasy.NewTextErrorResponse("limit must be between 0 and 100"), nil
			}

			view, failed, err := resolveDelegations(ctx, tasks, threads, AgentOutputToolName)
			if err != nil || failed != nil {
				return derefResponse(failed), err
			}
			ref, refusal := view.lookup(ctx, tasks, params.ID, "read")
			if refusal != nil {
				return *refusal, nil
			}
			if ref.Kind == KindThread {
				// A thread's session belongs to its own worktree
				// workspace, which this one has no message store for -
				// unlike a task, which runs on the parent's own store.
				return unsupported(ref, "a thread's transcript lives in its own worktree session, which is not readable from here",
					"Use agent_result for what it has reported, or open the thread to read it."), nil
			}

			out, err := tasks.Output(ctx, ref.Task.ID, params.Limit)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(renderTaskOutput(ref.Task, out)), out), nil
		},
	), map[string]toolParameterSchema{"id": {minLength: intPtr(1)}, "limit": intSchemaBounds(100)})
}

// renderTaskOutput renders out for the caller. The task's goal is left
// out when the transcript opens with it: the caller wrote it, and echoing
// it back made every check on a task that had not said anything yet cost
// the caller its whole prompt again. A task that has not finished ends
// with waitHint, since a caller reading its transcript is the one most
// likely to be polling it.
func renderTaskOutput(ti TaskInfo, out TaskOutput) string {
	msgs := out.Messages
	goalOmitted := false
	if len(msgs) == out.Total && len(msgs) > 0 && msgs[0].Role == "user" && msgs[0].Text == ti.Goal {
		msgs = msgs[1:]
		goalOmitted = true
	}

	var sb strings.Builder
	switch {
	case len(msgs) == 0 && goalOmitted:
		sb.WriteString("No output yet beyond the goal you gave it.")
	case len(msgs) == 0:
		sb.WriteString("No output yet.")
	default:
		if out.Total > len(out.Messages) {
			fmt.Fprintf(&sb, "Showing last %d of %d messages.\n\n", len(out.Messages), out.Total)
		}
		if goalOmitted {
			sb.WriteString("[user] (the goal you gave it, omitted)\n\n")
		}
		for _, m := range msgs {
			fmt.Fprintf(&sb, "[%s] %s\n\n", m.Role, m.Text)
		}
	}
	if !taskFinished(ti) {
		fmt.Fprintf(&sb, "\n\nTask %s is still %s. %s", ti.ID, ti.Status, waitHint)
	}
	return strings.TrimSpace(sb.String())
}
