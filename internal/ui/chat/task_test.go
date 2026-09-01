package chat

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/message"
	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// pipelineGoal is the shape a pipeline skill gives a delegation: the agent's
// role first, the work second. Rendering the first line verbatim shows the
// role and never the work.
const pipelineGoal = "ROLE: reviewer\n" +
	"TASK: C5 providerload extraction\n" +
	"ORIGINAL USER REQUEST:\n" +
	"продолжай\n"

func renderTaskTool(t *testing.T, toolName, input string, metadata, content string) string {
	t.Helper()
	sty := styles.SennitDark()
	call := message.ToolCall{ID: "tc-1", Name: toolName, Input: input, Finished: true}
	result := &message.ToolResult{ToolCallID: "tc-1", Content: content, Metadata: metadata}
	return ansi.Strip(NewToolMessageItem(&sty, "msg", call, result, false, nil).Render(120))
}

func taskInfoJSON(t *testing.T, info taskInfo) string {
	t.Helper()
	data, err := json.Marshal(info)
	require.NoError(t, err)
	return string(data)
}

// TestTaskResultRender covers the line this renderer exists for. The generic
// renderer had only the call's arguments, so it printed the task's uuid and
// a line count — the two facts in reach and the two nobody can use.
func TestTaskResultRender(t *testing.T) {
	t.Parallel()

	out := renderTaskTool(t, tools.AgentResultToolName,
		`{"id":"a8355bfc-08c7-4024-8af1-b157a4f836ff"}`,
		taskInfoJSON(t, taskInfo{Goal: pipelineGoal, Status: "running"}),
		"Task a8355bfc-08c7-4024-8af1-b157a4f836ff is still running; no result yet.")

	require.Contains(t, out, "C5 providerload extraction", "the header must say which work was asked about")
	require.Contains(t, out, "running", "and what came back")
	require.NotContains(t, out, "a8355bfc", "the uuid identifies the task to the model, not to a reader")
	require.NotContains(t, out, "ROLE:", "the role names the agent, not the work")
	require.NotContains(t, out, "1 line")
}

// TestTaskResultRender_Failure covers the one case where the status alone is
// not the whole story.
func TestTaskResultRender_Failure(t *testing.T) {
	t.Parallel()

	out := renderTaskTool(t, tools.AgentResultToolName, `{"id":"t1"}`,
		taskInfoJSON(t, taskInfo{Goal: "look into the flaky test", Status: "failed", Error: "context deadline exceeded\nstack..."}),
		"Task t1 did not complete (status=failed): context deadline exceeded")

	require.Contains(t, out, "look into the flaky test")
	require.Contains(t, out, "failed")
	require.Contains(t, out, "context deadline exceeded")
	require.NotContains(t, out, "stack...", "one line of the reason, not the whole of it")
}

// TestTaskToolRender_Subjects walks the rest of the family: each says what
// it did to which task, and none of them prints a uuid.
func TestTaskToolRender_Subjects(t *testing.T) {
	t.Parallel()

	t.Run("task_output reports how much of the transcript came back", func(t *testing.T) {
		t.Parallel()
		out := renderTaskTool(t, tools.AgentOutputToolName, `{"id":"t1","limit":20}`,
			`{"Messages":[{"Role":"assistant","Text":"looking"}],"Total":37}`, "[assistant] looking")
		require.Contains(t, out, "last 1 of 37 messages")
	})

	t.Run("task_output with nothing yet says so", func(t *testing.T) {
		t.Parallel()
		out := renderTaskTool(t, tools.AgentOutputToolName, `{"id":"t1"}`, `{"Messages":null,"Total":0}`, "No output yet.")
		require.Contains(t, out, "no output yet")
	})

	t.Run("task_list counts what is still moving", func(t *testing.T) {
		t.Parallel()
		meta := `{"Tasks":[{"Status":"running"},{"Status":"running"},{"Status":"completed"}]}`
		out := renderTaskTool(t, tools.AgentListToolName, `{}`, meta, "…")
		require.Contains(t, out, "3 tasks, 2 running")
	})

	t.Run("task_cancel names the task it stopped", func(t *testing.T) {
		t.Parallel()
		out := renderTaskTool(t, tools.AgentCancelToolName, `{"id":"t1"}`,
			taskInfoJSON(t, taskInfo{Goal: pipelineGoal, Status: "cancelled"}), "Task t1 status: cancelled")
		require.Contains(t, out, "C5 providerload extraction")
		require.Contains(t, out, "cancelled")
	})

	t.Run("task_send shows the instruction it sent", func(t *testing.T) {
		t.Parallel()
		// No metadata: what it did is in its text, so the subject has to
		// come from the call.
		out := renderTaskTool(t, tools.AgentSendToolName,
			`{"id":"t1","message":"drop the modelcache move\nand rerun the tests"}`, "", "Queued for task t1.")
		require.Contains(t, out, "drop the modelcache move")
		require.NotContains(t, out, "rerun the tests", "the header is one line")
	})
}

// TestTaskToolRender_NoMetadata proves the renderer degrades to saying less
// rather than to saying something wrong: a call that failed before the task
// manager answered has nothing to report but its own name.
func TestTaskToolRender_NoMetadata(t *testing.T) {
	t.Parallel()

	out := renderTaskTool(t, tools.AgentResultToolName, `{"id":"t1"}`, "", "no such task")
	require.Contains(t, out, "Agent Result")
	require.NotContains(t, out, "t1")
}

// TestTaskGoalHeadline covers the goal-to-headline rule on its own, since it
// is the part with a judgement in it.
func TestTaskGoalHeadline(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, goal, want string }{
		{"a pipeline goal names the work, not the role", pipelineGoal, "C5 providerload extraction"},
		{"an ordinary goal is its own first line", "look into the flaky test\nand report", "look into the flaky test"},
		{"prose with a colon is not a label", "Fix this: the parser drops newlines", "Fix this: the parser drops newlines"},
		{"an empty goal stays empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, taskGoalHeadline(tc.goal))
		})
	}
}
