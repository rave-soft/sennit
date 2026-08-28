package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

func runJobOutputTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params JobOutputParams) fantasy.ToolResponse {
	t.Helper()

	input, err := json.Marshal(params)
	require.NoError(t, err)

	call := fantasy.ToolCall{
		ID:    "test-call",
		Name:  JobOutputToolName,
		Input: string(input),
	}

	resp, err := tool.Run(ctx, call)
	require.NoError(t, err)
	return resp
}

// TestJobOutputTool_WaitInterruptedByUserInput checks that wait=true on a
// still-running job returns promptly, with the interruption marker, once
// the context's user-input signal fires — instead of blocking until the
// job itself finishes.
func TestJobOutputTool_WaitInterruptedByUserInput(t *testing.T) {
	bgManager := shell.NewBackgroundShellManager()
	tool := NewJobOutputTool(bgManager)

	// A job that outlives the test's patience if actually waited on.
	bgShell, err := bgManager.Start(context.Background(), t.TempDir(), nil, "sleep 5", "slow job")
	require.NoError(t, err)

	closed := make(chan struct{})
	close(closed)
	ctx := WithUserInput(context.Background(), func() <-chan struct{} { return closed })

	start := time.Now()
	resp := runJobOutputTool(t, tool, ctx, JobOutputParams{ShellID: bgShell.ID, Wait: true})
	elapsed := time.Since(start)

	require.Less(t, elapsed, 2*time.Second, "wait should have been cut short, not run to completion")
	require.Contains(t, resp.Content, "Wait interrupted")

	var meta JobOutputResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, "running", meta.Status)
	require.False(t, meta.Done)
	require.Equal(t, bgShell.ID, meta.ShellID)

	_ = bgManager.Kill(bgShell.ID)
}

// TestJobOutputTool_WaitOutsideSessionRunsToCompletion checks that, with no
// user-input signal installed in the context (the tool running outside a
// session), wait=true behaves exactly as before: it blocks until the job
// finishes and reports the ordinary "completed" result.
func TestJobOutputTool_WaitOutsideSessionRunsToCompletion(t *testing.T) {
	bgManager := shell.NewBackgroundShellManager()
	tool := NewJobOutputTool(bgManager)

	bgShell, err := bgManager.Start(context.Background(), t.TempDir(), nil, "echo hi", "fast job")
	require.NoError(t, err)

	resp := runJobOutputTool(t, tool, context.Background(), JobOutputParams{ShellID: bgShell.ID, Wait: true})

	require.NotContains(t, resp.Content, "Wait interrupted")

	var meta JobOutputResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, "completed", meta.Status)
	require.True(t, meta.Done)
}

// TestJobOutputTool_WaitPrefersReadyResultOverInterruption checks the race
// between the two select cases: when the job is already done AND the
// user-input signal is already closed by the time job_output selects on
// them, a result in hand must win over declaring an interruption, since
// otherwise the finished job's output would be thrown away for no benefit
// (the turn ends either way). Both channels are made ready before the tool
// call — bgShell.Wait() blocks until the job's done channel is actually
// closed, and the user-input channel is pre-closed — so Go's random choice
// among ready select cases would otherwise make this test's outcome flip
// from run to run; run with -count=20 (or more) to keep it honest.
func TestJobOutputTool_WaitPrefersReadyResultOverInterruption(t *testing.T) {
	bgManager := shell.NewBackgroundShellManager()
	tool := NewJobOutputTool(bgManager)

	bgShell, err := bgManager.Start(context.Background(), t.TempDir(), nil, "echo hi", "fast job")
	require.NoError(t, err)
	bgShell.Wait() // block until bgShell.Done() is actually closed
	require.True(t, bgShell.IsDone())

	closed := make(chan struct{})
	close(closed)
	ctx := WithUserInput(context.Background(), func() <-chan struct{} { return closed })

	resp := runJobOutputTool(t, tool, ctx, JobOutputParams{ShellID: bgShell.ID, Wait: true})

	require.NotContains(t, resp.Content, "Wait interrupted")

	var meta JobOutputResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, "completed", meta.Status)
	require.True(t, meta.Done)
	require.Contains(t, resp.Content, "hi")
}

// TestJobOutputTool_WaitCompletesBeforeUserInputSignals checks that a job
// finishing on its own, with a user-input signal present but never firing,
// still produces the ordinary "completed" result rather than the
// interruption marker.
func TestJobOutputTool_WaitCompletesBeforeUserInputSignals(t *testing.T) {
	bgManager := shell.NewBackgroundShellManager()
	tool := NewJobOutputTool(bgManager)

	bgShell, err := bgManager.Start(context.Background(), t.TempDir(), nil, "echo hi", "fast job")
	require.NoError(t, err)

	// A signal that never closes: present, but does not fire before the
	// job finishes on its own.
	never := make(chan struct{})
	ctx := WithUserInput(context.Background(), func() <-chan struct{} { return never })

	resp := runJobOutputTool(t, tool, ctx, JobOutputParams{ShellID: bgShell.ID, Wait: true})

	require.NotContains(t, resp.Content, "Wait interrupted")

	var meta JobOutputResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, "completed", meta.Status)
	require.True(t, meta.Done)
}
