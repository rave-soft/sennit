package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/message"
	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

func renderJobOutput(t *testing.T, params tools.JobOutputParams, meta tools.JobOutputResponseMetadata, content string) string {
	t.Helper()
	sty := styles.SennitDark()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	metaJSON, err := json.Marshal(meta)
	require.NoError(t, err)
	call := message.ToolCall{ID: "tc-job", Name: tools.JobOutputToolName, Input: string(input), Finished: true}
	result := &message.ToolResult{ToolCallID: "tc-job", Content: content, Metadata: string(metaJSON)}
	return ansi.Strip(NewToolMessageItem(&sty, "msg", call, result, false, nil).Render(120))
}

// TestJobOutputRender covers the line this renderer exists for. It used to
// print "Job (Output) PID 008" and nothing else: the plumbing first, the
// work nowhere, and the output the call was made for not shown at all.
func TestJobOutputRender(t *testing.T) {
	t.Parallel()

	out := renderJobOutput(t,
		tools.JobOutputParams{ShellID: "008"},
		tools.JobOutputResponseMetadata{
			ShellID:     "008",
			Command:     "make test-integration",
			Description: "Запустить интеграционные тесты",
			Done:        true,
			Status:      "completed",
			Output:      "ok  	internal/agent	1.2s",
		},
		"Status: completed\n\nok  	internal/agent	1.2s")

	require.Contains(t, out, "Запустить интеграционные тесты", "the header must say which job")
	require.Contains(t, out, "done", "and how it ended")
	require.Contains(t, out, "internal/agent", "and show what the job printed")
	require.NotContains(t, out, "PID 008", "a shell id addresses the job to the model, not to a reader")
	require.NotContains(t, out, "(Output)")
	require.NotContains(t, out, "Status: completed", "the preamble is the header's job, not the body's")
}

// TestJobOutputRender_Outcomes walks the three endings a job has.
func TestJobOutputRender_Outcomes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		meta tools.JobOutputResponseMetadata
		want string
	}{
		{"still running", tools.JobOutputResponseMetadata{Status: "running", Command: "npm test"}, "running"},
		{"finished cleanly", tools.JobOutputResponseMetadata{Status: "completed", Done: true, Command: "npm test"}, "done"},
		{"finished badly", tools.JobOutputResponseMetadata{Status: "completed", Done: true, ExitCode: 1, Command: "npm test"}, "exit 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := renderJobOutput(t, tools.JobOutputParams{ShellID: "008"}, tc.meta, "…")
			require.Contains(t, out, "npm test")
			require.Contains(t, out, tc.want)
		})
	}
}

// TestJobOutputRender_OlderResults proves the renderer degrades rather than
// invents: a result stored before status/exit code/output were recorded
// still reports which job and whether it was still going, and recovers the
// body by dropping the preamble the tool wrote in front of it.
func TestJobOutputRender_OlderResults(t *testing.T) {
	t.Parallel()

	out := renderJobOutput(t,
		tools.JobOutputParams{ShellID: "008"},
		tools.JobOutputResponseMetadata{ShellID: "008", Command: "npm test", Done: true},
		"Status: completed\n\nsuites passed")

	require.Contains(t, out, "npm test")
	require.Contains(t, out, "done")
	require.Contains(t, out, "suites passed")
	require.NotContains(t, out, "Status: completed")
	require.NotContains(t, out, "exit ", "no exit code was recorded; a guessed one would read as fact")
}

// TestJobOutputRender_NoOutputYet: a job that has printed nothing gets a
// header and no body, not a body saying the tool's own placeholder.
func TestJobOutputRender_NoOutputYet(t *testing.T) {
	t.Parallel()

	out := renderJobOutput(t,
		tools.JobOutputParams{ShellID: "008"},
		tools.JobOutputResponseMetadata{ShellID: "008", Command: "npm test", Status: "running"},
		"Status: running\n\n"+tools.BashNoOutput)

	require.Len(t, strings.Split(strings.TrimRight(out, "\n"), "\n"), 1, "header only, got: %q", out)
	require.Contains(t, out, "running")
	require.NotContains(t, out, tools.BashNoOutput)
}

// TestJobOutputRender_UnknownJob falls back to the only identification
// left. It is a poor name for a job, but it is a true one.
func TestJobOutputRender_UnknownJob(t *testing.T) {
	t.Parallel()

	out := renderJobOutput(t, tools.JobOutputParams{ShellID: "008"}, tools.JobOutputResponseMetadata{}, "")
	require.Contains(t, out, "PID 008")
}

// TestJobStartRender covers the other half of the pair: the bash call that
// started the job. Its result text is an instruction to the model, so what
// the transcript shows is the job and its command instead.
func TestJobStartRender(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	input, err := json.Marshal(tools.BashParams{Command: "make test-integration", RunInBackground: true})
	require.NoError(t, err)
	meta, err := json.Marshal(tools.BashResponseMetadata{
		Background: true, ShellID: "008", Description: "Запустить интеграционные тесты",
	})
	require.NoError(t, err)
	call := message.ToolCall{ID: "tc-bash", Name: tools.BashToolName, Input: string(input), Finished: true}
	result := &message.ToolResult{
		ToolCallID: "tc-bash",
		Content:    "Background shell started with ID: 008\n\nUse job_output tool to view output or job_kill to terminate.",
		Metadata:   string(meta),
	}
	out := ansi.Strip(NewBashToolMessageItem(&sty, call, result, false).Render(120))

	require.Contains(t, out, "Запустить интеграционные тесты")
	require.Contains(t, out, "started")
	require.Contains(t, out, "make test-integration", "the command is the one fact the description may omit")
	require.NotContains(t, out, "job_output tool", "an instruction to the model is not news to a reader")
	require.NotContains(t, out, "PID 008")
}

// TestJobKillRender names what was stopped.
func TestJobKillRender(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	input, err := json.Marshal(tools.JobKillParams{ShellID: "008"})
	require.NoError(t, err)
	meta, err := json.Marshal(tools.JobKillResponseMetadata{
		ShellID: "008", Command: "make test-integration", Description: "None",
	})
	require.NoError(t, err)
	call := message.ToolCall{ID: "tc-kill", Name: tools.JobKillToolName, Input: string(input), Finished: true}
	result := &message.ToolResult{ToolCallID: "tc-kill", Content: "Background shell 008 terminated successfully"}
	result.Metadata = string(meta)
	out := ansi.Strip(NewToolMessageItem(&sty, "msg", call, result, false, nil).Render(120))

	require.Contains(t, out, "make test-integration", "a junk description falls back to the command")
	require.Contains(t, out, "killed")
	require.NotContains(t, strings.ToLower(out), "none")
}
