package tools

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAgentTraceDescriptionDocumentsPagination is the regression test for
// finding E: agent_trace had no description at all (an empty string
// literal) and none of its five params had a `description` tag, leaving a
// model with no way to learn about the session_id/run_id requirement, the
// limit bounds, or how to page with cursor/next_cursor.
func TestAgentTraceDescriptionDocumentsPagination(t *testing.T) {
	desc := agentTraceDescription()
	require.NotEmpty(t, desc)
	require.Contains(t, desc, "session_id")
	require.Contains(t, desc, "next_cursor")
	require.Contains(t, desc, "200", "must document the actual default limit")
	require.Contains(t, desc, "1000", "must document the actual max limit")

	typ := reflect.TypeOf(AgentTraceParams{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		require.NotEmpty(t, field.Tag.Get("description"), "field %s must have a description tag", field.Name)
	}
}

func TestAgentTraceFixturePaginationAndSafety(t *testing.T) {
	file := t.TempDir() + "/trace.jsonl"
	lines := []string{
		`{"time":"2024-01-01T00:00:00Z","msg":"Tool lifecycle","session_id":"s","run_id":"r","event":"tool_call","tool_call_id":"call-1","tool_name":"read","tool_outcome":"started","arguments":"TOP-SECRET"}`,
		`{"time":"2024-01-01T00:00:01Z","msg":"Provider request started","session_id":"s","run_id":"r","request_reason":"turn","attempt":1,"prompt":"TOP-SECRET"}`,
		`{"time":"2024-01-01T00:00:02Z","msg":"Provider request failed, retrying","session_id":"s","run_id":"r","retry_reason":"server_error","error":"TOP-SECRET"}`,
		`{"time":"2024-01-01T00:00:03Z","msg":"Provider request finished","session_id":"s","run_id":"r","outcome":"success","input_tokens":12,"output_tokens":3,"cache_read_tokens":4,"cache_creation_tokens":5,"latency_ms":42}`,
		`{"time":"2024-01-01T00:00:04Z","msg":"Tool lifecycle","session_id":"s","run_id":"r","event":"tool_result","tool_call_id":"call-1","tool_name":"read","tool_outcome":"error","result":"TOP-SECRET"}`,
		`{"time":"2024-01-01T00:00:05Z","msg":"Tool lifecycle","session_id":"s","run_id":"r","event":"tool_call","tool_call_id":"call-unpaired","tool_name":"write"}`,
		`{"time":"2024-01-01T00:00:06Z","msg":"ordinary diagnostic","session_id":"s","run_id":"r","secret":"TOP-SECRET"}`,
		`not json`,
	}
	require.NoError(t, os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o600))

	firstText, err := runAgentTrace(file, AgentTraceParams{SessionID: " s ", RunID: " r ", Limit: 2})
	require.NoError(t, err)
	var first agentTraceResponse
	require.NoError(t, json.Unmarshal([]byte(firstText), &first))
	require.Len(t, first.Events, 2)
	require.True(t, first.Truncated)
	require.NotEmpty(t, first.NextCursor)
	require.True(t, first.SummaryExact)
	require.Equal(t, 1, first.PageSummary.Success)
	require.Equal(t, 0, first.PageSummary.Errors)
	require.Equal(t, 1, first.PageSummary.UnpairedCalls, "pairing must cover records split across pages")
	require.Equal(t, 0, first.PageSummary.UnpairedResults)
	require.NotContains(t, firstText, "TOP-SECRET")

	all := append([]agentTraceEvent{}, first.Events...)
	cursor := first.NextCursor
	for cursor != "" {
		text, err := runAgentTrace(file, AgentTraceParams{SessionID: "s", RunID: "r", Limit: 2, Cursor: cursor})
		require.NoError(t, err)
		var page agentTraceResponse
		require.NoError(t, json.Unmarshal([]byte(text), &page))
		all = append(all, page.Events...)
		cursor = page.NextCursor
	}
	require.Len(t, all, 6)
	var retry, attemptFinished, toolCall, toolResult agentTraceEvent
	for _, event := range all {
		switch event.Kind {
		case "retry":
			retry = event
		case "attempt_finished":
			attemptFinished = event
		case "tool_call":
			if event.Tool == "read" {
				toolCall = event
			}
		case "tool_result":
			toolResult = event
		}
	}
	require.Equal(t, "server_error", retry.RetryReason)
	require.Equal(t, "success", attemptFinished.Outcome)
	require.EqualValues(t, 42, attemptFinished.LatencyMS)
	require.Equal(t, "started", toolCall.Outcome)
	require.Equal(t, "error", toolResult.Outcome)

	text, err := runAgentTrace(file, AgentTraceParams{SessionID: "s", Since: "2024-01-01T00:00:03Z"})
	require.NoError(t, err)
	require.NotContains(t, text, `"retry"`)
	_, err = runAgentTrace(file, AgentTraceParams{SessionID: "s", Cursor: first.NextCursor})
	require.Error(t, err, "cursor must be bound to its filter")
}

func TestAgentTraceRejectsStaleCursor(t *testing.T) {
	file := t.TempDir() + "/trace.jsonl"
	require.NoError(t, os.WriteFile(file, []byte(`{"msg":"Provider request started","session_id":"s"}`+"\n"+`{"msg":"Provider request finished","session_id":"s","outcome":"success"}`+"\n"), 0o600))
	text, err := runAgentTrace(file, AgentTraceParams{SessionID: "s", Limit: 1})
	require.NoError(t, err)
	var page agentTraceResponse
	require.NoError(t, json.Unmarshal([]byte(text), &page))
	require.NotEmpty(t, page.NextCursor)
	require.NoError(t, os.Remove(file))
	require.NoError(t, os.WriteFile(file, []byte(`{"msg":"Provider request started","session_id":"s"}`+"\n"), 0o600))
	stale, err := runAgentTrace(file, AgentTraceParams{SessionID: "s", Limit: 1, Cursor: page.NextCursor})
	require.NoError(t, err)
	require.JSONEq(t, `{"events":[],"page_summary":{"attempts":0,"success":0,"errors":0,"canceled":0,"aborted":0,"retries":0,"summaries":0,"trims":0,"orphan_repairs":0,"tool_calls":0,"tool_results":0,"unpaired_calls":0,"unpaired_results":0},"summary_exact":false,"truncated":false}`, stale)
}
