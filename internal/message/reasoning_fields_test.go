package message

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReasoningWritersPreserveProviderBookkeeping pins every writer that
// touches a ReasoningContent part against the mistake they all shared:
// rebuilding the part from a hand-written field list, which silently
// dropped whatever the list forgot. What it forgot was the provider
// bookkeeping — ThoughtSignature, ToolID, ResponsesData — so a turn that
// recorded a thought signature and Responses metadata and then called
// FinishThinking (exactly what internal/agent's turn does) had all of it
// erased before the message was ever flushed, and the reasoning item could
// not be resumed on the next turn.
func TestReasoningWritersPreserveProviderBookkeeping(t *testing.T) {
	t.Parallel()

	encrypted := "cipher"
	data := &ResponsesReasoningMetadata{ItemID: "item_1", EncryptedContent: &encrypted, Summary: []string{"line"}}

	m := &Message{}
	m.AppendReasoningContent("first ")
	m.AppendReasoningSignature("sig")
	m.AppendThoughtSignature("thought", "call-1")
	m.SetReasoningResponsesData(data)

	// Each of these runs after every field above is set, so any writer
	// that rebuilds from a field list loses something here.
	m.AppendReasoningContent("second")
	m.AppendReasoningSignature("-more")
	m.FinishThinking()

	got := m.ReasoningContent()
	require.Equal(t, "first second", got.Thinking)
	require.Equal(t, "sig-more", got.Signature)
	require.Equal(t, "thought", got.ThoughtSignature)
	require.Equal(t, "call-1", got.ToolID)
	require.Equal(t, data, got.ResponsesData)
	require.NotZero(t, got.FinishedAt)
}

// TestFinishThinkingKeepsResponsesData is the single-writer form of the
// case that actually bit: turn.go writes the Responses metadata and then
// finishes the thinking part in the same step.
func TestFinishThinkingKeepsResponsesData(t *testing.T) {
	t.Parallel()

	m := &Message{}
	m.AppendReasoningContent("thinking")
	m.SetReasoningResponsesData(&ResponsesReasoningMetadata{ItemID: "item_2"})
	m.FinishThinking()

	require.NotNil(t, m.ReasoningContent().ResponsesData)
	require.Equal(t, "item_2", m.ReasoningContent().ResponsesData.ItemID)
}
