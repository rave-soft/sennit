package tools

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// TestSennitLogsTool_PropagatesCancellationAsGoError is the regression test
// for group 3 of the "text response vs. Go error" rule: a canceled call
// used to run runSennitLogs to completion (or fail on its own I/O) and
// report the result as an ordinary text response either way. See
// AGENTS.md's "Tool failures: text response vs. Go error".
func TestSennitLogsTool_PropagatesCancellationAsGoError(t *testing.T) {
	t.Parallel()
	logFile := createTestLogFile(t, []map[string]any{makeLogEntry("INFO", "hello", "app.go", 1, nil)})
	tool := NewSennitLogsTool(logFile)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "1", Name: SennitLogsToolName, Input: "{}"})
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled))
	require.Equal(t, fantasy.ToolResponse{}, resp)
}

// TestSennitLogsTool_InvalidCursorStaysTextResponse pins the other side of
// the split: an invalid argument the model supplied is still something it
// can react to, so it must stay a text response even though the same tool
// now also returns Go errors for I/O and cancellation failures.
func TestSennitLogsTool_InvalidCursorStaysTextResponse(t *testing.T) {
	t.Parallel()
	logFile := createTestLogFile(t, []map[string]any{makeLogEntry("INFO", "hello", "app.go", 1, nil)})
	tool := NewSennitLogsTool(logFile)

	resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "1", Name: SennitLogsToolName, Input: mustJSONInput(t, SennitLogsParams{Cursor: "not-a-real-cursor"})})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "invalid cursor")
}

// TestAgentTraceTool_PropagatesCancellationAsGoError mirrors the
// sennit_logs case for agent_trace.go's own tool wrapper.
func TestAgentTraceTool_PropagatesCancellationAsGoError(t *testing.T) {
	t.Parallel()
	logFile := createTestLogFile(t, []map[string]any{makeLogEntry("INFO", "hello", "app.go", 1, map[string]any{"session_id": "s"})})
	tool := NewAgentTraceTool(logFile)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "1", Name: AgentTraceToolName, Input: mustJSONInput(t, AgentTraceParams{SessionID: "s"})})
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled))
	require.Equal(t, fantasy.ToolResponse{}, resp)
}

// TestAgentTraceTool_MissingSelectorStaysTextResponse pins the other side
// of the split for agent_trace: a caller argument mistake stays text.
func TestAgentTraceTool_MissingSelectorStaysTextResponse(t *testing.T) {
	t.Parallel()
	logFile := createTestLogFile(t, []map[string]any{makeLogEntry("INFO", "hello", "app.go", 1, nil)})
	tool := NewAgentTraceTool(logFile)

	resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "1", Name: AgentTraceToolName, Input: "{}"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "session_id or run_id is required")
}
