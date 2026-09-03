package chat

import (
	"encoding/json"
	"fmt"
	"strings"

	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// -----------------------------------------------------------------------------
// Task tools
// -----------------------------------------------------------------------------

// A task_* tool call's arguments are just a task id — a uuid identifies
// the task to the model, which has just read it out of a tool result, but
// it identifies nothing to a person reading the transcript, who never
// sees one anywhere else. What they want is which delegation was asked
// about and what came back, so this renderer reads the task's own record
// (goal, status, result summary) off the response metadata instead.

// registerTaskToolRenderers registers the renderer for every agent_*
// delegation tool. They share one because they share a subject — one
// delegation, addressed by id — and differ only in what they did to it.
func registerTaskToolRenderers() {
	for _, name := range []string{
		tools.AgentResultToolName,
		tools.AgentOutputToolName,
		tools.AgentListToolName,
		tools.AgentCancelToolName,
		tools.AgentSendToolName,
		// The earlier per-kind names: sessions recorded before these tools
		// were merged into agent_* still hold calls under them and must
		// keep rendering; the subject each one reads comes from metadata
		// attached in the same shape.
		"task_result", "task_output", "task_list", "task_cancel", "task_send",
		"thread_list", "thread_status", "thread_send",
	} {
		registerToolRenderer(name, &TaskToolRenderContext{})
	}
}

// taskInfo mirrors the fields of internal/agent/tools.TaskInfo the
// transcript needs. That type carries no JSON tags, so its metadata is
// written with Go field names — matched here exactly rather than by
// retagging the source, which would orphan every result already stored.
type taskInfo struct {
	Goal          string
	Status        string
	ResultSummary string
	Error         string
}

// taskListMetadata mirrors tools.AgentListResponseMetadata. Only the task
// half is read: a thread row renders from its own fields, and the count
// line this feeds is about background tasks.
type taskListMetadata struct {
	Tasks []taskInfo
}

// taskOutputMetadata mirrors tools.TaskOutput: how much of a task's
// transcript came back, and how much there was.
type taskOutputMetadata struct {
	Messages []struct{ Role, Text string }
	Total    int
}

// TaskToolRenderContext renders the task_* tools.
type TaskToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (t *TaskToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	name := humanizedToolName(opts.ToolCall.Name)
	if opts.IsPending() {
		return pendingTool(sty, name, opts)
	}

	subject, summary := taskToolSubject(opts)
	header := toolHeader(sty, opts.Status, name, width, opts, subject)
	if opts.Compact {
		return header
	}
	if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
		return joinToolParts(header, earlyState)
	}
	return appendResultSummary(sty, header, summary)
}

// taskToolSubject reads a finished task_* call's arguments and response
// metadata into the two things its header line is made of: the subject (the
// delegation being asked about, said the way a person would say it) and a
// short summary of what came back.
//
// Both may be empty. A task tool that failed before the manager answered
// has no metadata to read, and toolHeader/appendResultSummary each drop an
// empty part rather than printing a placeholder.
func taskToolSubject(opts *ToolRenderOpts) (subject, summary string) {
	metadata := ""
	if opts.HasResult() {
		metadata = opts.Result.Metadata
	}

	switch opts.ToolCall.Name {
	case tools.AgentListToolName:
		var meta taskListMetadata
		if json.Unmarshal([]byte(metadata), &meta) != nil || len(meta.Tasks) == 0 {
			return "", "no tasks"
		}
		return "", taskCountSummary(meta.Tasks)

	case tools.AgentOutputToolName:
		var meta taskOutputMetadata
		if json.Unmarshal([]byte(metadata), &meta) != nil || meta.Total == 0 {
			return "", "no output yet"
		}
		if len(meta.Messages) < meta.Total {
			return "", fmt.Sprintf("last %d of %d messages", len(meta.Messages), meta.Total)
		}
		return "", fmt.Sprintf("%d messages", meta.Total)

	case tools.AgentSendToolName:
		// agent_send attaches no metadata — what it did is in its text —
		// so the subject comes from the call itself: the instruction
		// sent, which is the part a reader would recognize.
		var params struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)
		return firstLine(strings.TrimSpace(params.Message)), ""

	default:
		var info taskInfo
		if json.Unmarshal([]byte(metadata), &info) != nil {
			return "", ""
		}
		return taskGoalHeadline(info.Goal), taskOutcomeSummary(info)
	}
}

// taskOutcomeSummary says what became of a task in as few words as the
// facts allow: its status, and for one that failed, why. A completed task's
// own result summary is deliberately not repeated here — the delegation's
// own block in this same transcript is where the answer is read.
func taskOutcomeSummary(info taskInfo) string {
	if info.Status == "" {
		return ""
	}
	if info.Error != "" {
		return info.Status + ": " + firstLine(info.Error)
	}
	return info.Status
}

// taskCountSummary counts a task list by status, so "3 tasks" becomes
// something worth reading: what a person checks a list for is how much is
// still moving.
func taskCountSummary(tasks []taskInfo) string {
	running := 0
	for _, task := range tasks {
		if task.Status == "running" {
			running++
		}
	}
	if running == 0 || running == len(tasks) {
		return fmt.Sprintf("%d tasks", len(tasks))
	}
	return fmt.Sprintf("%d tasks, %d running", len(tasks), running)
}

// taskGoalHeadline summarizes a delegation's goal for a task tool's header.
//
// A goal written by a pipeline skill opens with "ROLE: <agent>", naming the
// agent rather than the work. So the role is read off the first label and
// then handed to delegationHeadline as the name to deduplicate against,
// which walks past that line and returns the one that says what the task
// actually is. A goal with no such structure is its own first line, as it
// always was.
func taskGoalHeadline(goal string) string {
	role := ""
	for _, line := range strings.Split(goal, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if value, ok := promptLabelValue(line); ok {
			role = value
		}
		break
	}
	return delegationHeadline(role, goal)
}
