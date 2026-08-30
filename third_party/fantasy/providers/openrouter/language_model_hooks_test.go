package openrouter

import (
	"encoding/json"
	"fmt"
	"testing"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/require"

	"charm.land/fantasy"
)

// TestLanguageModelExtraContent_NonContiguousReasoningIndexDoesNotPanic
// pins that a non-contiguous reasoning_details index (a first detail
// already at index 1, or a gap) does not panic. The parser appended
// exactly one element to the block slice regardless of the actual gap and
// then indexed it directly with detail.Index, which panicked with "index
// out of range" the moment an index arrived ahead of the slice's length.
// Covers both the openai-responses and google-gemini block kinds, which
// shared the same pattern.
func TestLanguageModelExtraContent_NonContiguousReasoningIndexDoesNotPanic(t *testing.T) {
	t.Parallel()

	t.Run("openai-responses", func(t *testing.T) {
		t.Parallel()

		choice := reasoningChoice(t, `[
			{"index": 1, "format": "openai-responses", "type": "reasoning.summary", "summary": "second"},
			{"index": 0, "format": "openai-responses", "type": "reasoning.summary", "summary": "first"}
		]`)

		var content []fantasy.Content
		require.NotPanics(t, func() {
			content = languageModelExtraContent(choice)
		})

		require.Len(t, content, 2)
		rc0, ok := content[0].(fantasy.ReasoningContent)
		require.True(t, ok)
		require.Equal(t, "first", rc0.Text)
		rc1, ok := content[1].(fantasy.ReasoningContent)
		require.True(t, ok)
		require.Equal(t, "second", rc1.Text)
	})

	t.Run("google-gemini", func(t *testing.T) {
		t.Parallel()

		choice := reasoningChoice(t, `[
			{"index": 1, "format": "google-gemini", "type": "reasoning.text", "text": "second"},
			{"index": 0, "format": "google-gemini", "type": "reasoning.text", "text": "first"}
		]`)

		var content []fantasy.Content
		require.NotPanics(t, func() {
			content = languageModelExtraContent(choice)
		})

		require.Len(t, content, 2)
		rc0, ok := content[0].(fantasy.ReasoningContent)
		require.True(t, ok)
		require.Equal(t, "first", rc0.Text)
		rc1, ok := content[1].(fantasy.ReasoningContent)
		require.True(t, ok)
		require.Equal(t, "second", rc1.Text)
	})
}

// reasoningChoice builds an openaisdk.ChatCompletionChoice whose
// Message.RawJSON() carries the given reasoning_details array, matching
// the shape languageModelExtraContent parses via
// json.Unmarshal(choice.Message.RawJSON(), &ReasoningData{}).
func reasoningChoice(t *testing.T, reasoningDetailsJSON string) openaisdk.ChatCompletionChoice {
	t.Helper()

	messageJSON := fmt.Sprintf(`{"role":"assistant","content":"","reasoning_details":%s}`, reasoningDetailsJSON)
	choiceJSON := fmt.Sprintf(`{"index":0,"finish_reason":"stop","message":%s}`, messageJSON)

	var choice openaisdk.ChatCompletionChoice
	require.NoError(t, json.Unmarshal([]byte(choiceJSON), &choice))
	return choice
}

// TestLanguageModelStreamExtra_FormatSwitchMidStreamDoesNotPanic pins that
// currentReasoningState.metadata/googleMetadata are allocated lazily. The
// Reasoning Start block only allocates the field matching the *first*
// detail's format; a later chunk whose detail switches to openai-responses
// or google-gemini (after starting on a different format, e.g.
// anthropic-claude) then dereferenced the still-nil field.
func TestLanguageModelStreamExtra_FormatSwitchMidStreamDoesNotPanic(t *testing.T) {
	t.Parallel()

	yield := func(fantasy.StreamPart) bool { return true }

	t.Run("openai-responses detail after an anthropic-claude start", func(t *testing.T) {
		t.Parallel()

		ctx := map[string]any{}
		require.NotPanics(t, func() {
			var cont bool
			ctx, cont = languageModelStreamExtra(reasoningChunk(t, `[
				{"index": 0, "format": "anthropic-claude", "text": "thinking"}
			]`), yield, ctx)
			require.True(t, cont)
			ctx, cont = languageModelStreamExtra(reasoningChunk(t, `[
				{"index": 0, "format": "openai-responses", "type": "reasoning.summary", "summary": "more"}
			]`), yield, ctx)
			require.True(t, cont)
		})
	})

	t.Run("google-gemini detail after an anthropic-claude start", func(t *testing.T) {
		t.Parallel()

		ctx := map[string]any{}
		require.NotPanics(t, func() {
			var cont bool
			ctx, cont = languageModelStreamExtra(reasoningChunk(t, `[
				{"index": 0, "format": "anthropic-claude", "text": "thinking"}
			]`), yield, ctx)
			require.True(t, cont)
			ctx, cont = languageModelStreamExtra(reasoningChunk(t, `[
				{"index": 0, "format": "google-gemini", "type": "reasoning.encrypted", "data": "cipher", "id": "tool-1"}
			]`), yield, ctx)
			require.True(t, cont)
		})
	})
}

// reasoningChunk builds an openaisdk.ChatCompletionChunk whose
// Choices[0].Delta.RawJSON() carries the given reasoning_details array,
// matching the shape languageModelStreamExtra parses via
// json.Unmarshal(choice.Delta.RawJSON(), &ReasoningData{}).
func reasoningChunk(t *testing.T, reasoningDetailsJSON string) openaisdk.ChatCompletionChunk {
	t.Helper()

	deltaJSON := fmt.Sprintf(`{"role":"assistant","content":"","reasoning_details":%s}`, reasoningDetailsJSON)
	choiceJSON := fmt.Sprintf(`{"index":0,"delta":%s}`, deltaJSON)
	chunkJSON := fmt.Sprintf(`{"id":"chunk-1","created":0,"model":"m","choices":[%s]}`, choiceJSON)

	var chunk openaisdk.ChatCompletionChunk
	require.NoError(t, json.Unmarshal([]byte(chunkJSON), &chunk))
	return chunk
}
