package agent

import (
	"encoding/json"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// delegationExecutionGolden is byte-for-byte what HEAD's code (before
// message.Message grew its own MarshalJSON/UnmarshalJSON) produced for
// a DelegationExecution carrying one history message with every field
// set — PascalCase keys throughout, including the Message value nested
// inside delegationHistoryMessage. This is the shape already sitting in
// the threads table's execution column (internal/db/threads.sql.go's
// Execution field) for every delegation created before this change, and
// a thread resume (internal/thread/task_resume.go) must keep decoding
// it correctly.
const delegationExecutionGolden = `{"AgentID":"agent-1","ParentSessionID":"parent-1","SessionID":"session-1","SessionTitle":"title","Goal":"goal","Depth":1,"Definition":{"id":"coder"},"Model":{"model":"gpt-5","provider":"openai"},"History":[[{"Message":{"ID":"msg-1","Role":"assistant","SessionID":"session-1","Parts":null,"Model":"gpt-5","Provider":"openai","CreatedAt":111,"UpdatedAt":222,"IsSummaryMessage":true,"Origin":"agent","SummaryBeforeTokens":333,"SummaryAfterTokens":444},"Parts":[{"type":"_meta","data":{"version":1}},{"type":"text","data":{"text":"hi"}}]}]],"HistoryFrozen":true,"Options":{"DisableAutoSummarize":true,"AutoSummarizeAt":5}}`

func delegationExecutionGoldenValue(t *testing.T) DelegationExecution {
	t.Helper()

	msg := message.Message{
		ID:                  "msg-1",
		Role:                message.Assistant,
		SessionID:           "session-1",
		Model:               "gpt-5",
		Provider:            "openai",
		CreatedAt:           111,
		UpdatedAt:           222,
		IsSummaryMessage:    true,
		Origin:              message.OriginAgent,
		SummaryBeforeTokens: 333,
		SummaryAfterTokens:  444,
	}
	parts, err := message.MarshalParts([]message.ContentPart{message.TextContent{Text: "hi"}})
	require.NoError(t, err)

	return DelegationExecution{
		AgentID:         "agent-1",
		ParentSessionID: "parent-1",
		SessionID:       "session-1",
		SessionTitle:    "title",
		Goal:            "goal",
		Depth:           1,
		Definition:      config.Agent{ID: "coder"},
		Model:           config.SelectedModel{Provider: "openai", Model: "gpt-5"},
		History: [][]delegationHistoryMessage{
			{
				{Message: delegationMessage(msg), Parts: parts},
			},
		},
		HistoryFrozen: true,
		Options:       DelegationRuntimeOptions{DisableAutoSummarize: true, AutoSummarizeAt: 5},
	}
}

// TestDelegationExecution_DecodesTodaysWireFormat decodes a literal
// Execution string in today's on-disk format (see
// delegationExecutionGolden's doc comment) and requires every field —
// including every message.Message field nested inside the history — to
// come back intact. A DelegationExecution that used message.Message's
// new snake_case, parts-embedding codec instead of the plain-reflection
// one would silently drop SessionID, CreatedAt, UpdatedAt,
// IsSummaryMessage, SummaryBeforeTokens and SummaryAfterTokens here,
// because encoding/json's case-insensitive field match maps "ID" and
// "Role" onto message.Message's snake_case tags but not e.g.
// "SessionID" onto "session_id".
func TestDelegationExecution_DecodesTodaysWireFormat(t *testing.T) {
	t.Parallel()

	var got DelegationExecution
	require.NoError(t, json.Unmarshal([]byte(delegationExecutionGolden), &got))

	require.Equal(t, delegationExecutionGoldenValue(t), got)
}

// TestDelegationExecution_EncodesTodaysWireFormat requires that encoding
// a DelegationExecution with the current code reproduces
// delegationExecutionGolden byte for byte, so the snapshot written to
// the threads table does not change shape out from under rows already
// on disk.
func TestDelegationExecution_EncodesTodaysWireFormat(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(delegationExecutionGoldenValue(t))
	require.NoError(t, err)

	require.JSONEq(t, delegationExecutionGolden, string(data))
}
