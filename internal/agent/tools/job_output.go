package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/shell"
)

const (
	JobOutputToolName = "job_output"
)

//go:embed job_output.md
var jobOutputDescription string

type JobOutputParams struct {
	ShellID string `json:"shell_id" description:"The ID of the background shell to retrieve output from"`
	Wait    bool   `json:"wait" description:"If true, block until the background shell completes before returning output"`
}

type JobOutputResponseMetadata struct {
	ShellID          string `json:"shell_id"`
	Command          string `json:"command"`
	Description      string `json:"description"`
	Done             bool   `json:"done"`
	WorkingDirectory string `json:"working_directory"`
	// Status is the word the model reads at the top of the result:
	// "running" or "completed". ExitCode is the process's, and is only
	// meaningful once Done. Output is what the job has printed so far,
	// without the status preamble — the transcript shows it as the
	// tool's body, so it must not have to parse the model's text back
	// apart to find it.
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
}

// interruptedJobOutputResponse builds the response for a wait cut short
// because the person sent a new message. It must read as unmistakably
// different from a plain "Status: running" poll: the model needs to
// understand the wait was broken on purpose so it hands the turn back to
// the person instead of calling job_output again and re-arming the same
// wait, which would only reproduce the hang this exists to avoid. The
// background shell itself is untouched — its ID keeps working for a later
// job_output call once the person has been answered.
func interruptedJobOutputResponse(bgShell *shell.BackgroundShell) fantasy.ToolResponse {
	metadata := JobOutputResponseMetadata{
		ShellID:          bgShell.ID,
		Command:          bgShell.Command,
		Description:      bgShell.Description,
		Done:             false,
		WorkingDirectory: bgShell.WorkingDir,
		Status:           "running",
	}
	result := fmt.Sprintf(
		"Status: running\n\nWait interrupted: the person sent a new message "+
			"while this was waiting for job %s to finish. Stop waiting and "+
			"respond to them now — do not call job_output again to keep "+
			"waiting. The background job is still running and its ID "+
			"remains valid; check on it with job_output later.",
		bgShell.ID,
	)
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata)
}

func NewJobOutputTool(bgManager *shell.BackgroundShellManager) fantasy.AgentTool {
	if bgManager == nil {
		panic("background shell manager is required")
	}
	return withToolParameterSchema(fantasy.NewAgentTool(
		JobOutputToolName,
		jobOutputDescription,
		func(ctx context.Context, params JobOutputParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ShellID == "" {
				return invalidParam("shell_id"), nil
			}

			bgShell, ok := bgManager.Get(params.ShellID)
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("background shell not found: %s", params.ShellID)), nil
			}

			if params.Wait {
				// WaitForUserInput returns nil outside a session (no signal
				// installed in the context); selecting on a nil channel
				// never fires, so this degrades to a plain wait for
				// bgShell.Done() in that case — no special-casing needed.
				select {
				case <-bgShell.Done():
				case <-WaitForUserInput(ctx):
					// Both channels can be ready at once (the job finished
					// right as the person typed); Go picks among ready
					// cases at random, so re-check with the non-blocking
					// IsDone before declaring an interruption. A result
					// already in hand is strictly better to return than to
					// discard, and it costs the model nothing since the
					// turn ends here either way.
					if !bgShell.IsDone() {
						return interruptedJobOutputResponse(bgShell), nil
					}
				case <-ctx.Done():
				}
			}

			stdout, stderr, done, err := bgShell.GetOutput()

			var outputParts []string
			if stdout != "" {
				outputParts = append(outputParts, stdout)
			}
			if stderr != "" {
				outputParts = append(outputParts, stderr)
			}

			status := "running"
			exitCode := 0
			if done {
				status = "completed"
				if err != nil {
					exitCode = shell.ExitCode(err)
					if exitCode != 0 {
						outputParts = append(outputParts, fmt.Sprintf("Exit code %d", exitCode))
					}
				}
			}

			output := strings.Join(outputParts, "\n")
			output = TruncateOutput(output)

			metadata := JobOutputResponseMetadata{
				ShellID:          params.ShellID,
				Command:          bgShell.Command,
				Description:      bgShell.Description,
				Done:             done,
				WorkingDirectory: bgShell.WorkingDir,
				Status:           status,
				ExitCode:         exitCode,
				Output:           output,
			}

			if output == "" {
				output = BashNoOutput
			}

			result := fmt.Sprintf("Status: %s\n\n%s", status, output)
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil
		},
	), map[string]toolParameterSchema{"shell_id": {minLength: intPtr(1)}})
}
