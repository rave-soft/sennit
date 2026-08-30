package vercel

import (
	"encoding/json"
	"fmt"
	"testing"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/require"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/google"
	openaipkg "charm.land/fantasy/providers/openai"
)

// TestLanguageModelExtraContent_NonContiguousReasoningIndexDoesNotPanic pins
// that a non-contiguous reasoning_details index (a first detail already at
// index 1, or a gap) does not panic. languageModelExtraContent appended
// exactly one element to the block slice regardless of the actual gap and
// then indexed it directly with detail.Index, which panicked with "index
// out of range" the moment an index arrived ahead of the slice's length.
// Covers both the openai-responses and google-gemini block kinds, which
// share the same pattern (see the identical fix in the openrouter provider).
func TestLanguageModelExtraContent_NonContiguousReasoningIndexDoesNotPanic(t *testing.T) {
	t.Parallel()

	t.Run("openai-responses index ahead of slice length", func(t *testing.T) {
		t.Parallel()

		choice := reasoningChoice(t, `[
			{"index": 1, "format": "openai-responses", "type": "reasoning.summary", "summary": "second"}
		]`)

		var content []fantasy.Content
		require.NotPanics(t, func() {
			content = languageModelExtraContent(choice)
		})

		require.Len(t, content, 2)
		rc0, ok := content[0].(fantasy.ReasoningContent)
		require.True(t, ok)
		require.Equal(t, "", rc0.Text)
		rc1, ok := content[1].(fantasy.ReasoningContent)
		require.True(t, ok)
		require.Equal(t, "second", rc1.Text)
	})

	t.Run("openai-responses gap between details", func(t *testing.T) {
		t.Parallel()

		choice := reasoningChoice(t, `[
			{"index": 0, "format": "openai-responses", "type": "reasoning.summary", "summary": "first"},
			{"index": 2, "format": "openai-responses", "type": "reasoning.encrypted", "data": "cipher"}
		]`)

		var content []fantasy.Content
		require.NotPanics(t, func() {
			content = languageModelExtraContent(choice)
		})

		require.Len(t, content, 3)
		rc0, ok := content[0].(fantasy.ReasoningContent)
		require.True(t, ok)
		require.Equal(t, "first", rc0.Text)
		rc2, ok := content[2].(fantasy.ReasoningContent)
		require.True(t, ok)
		meta, ok := rc2.ProviderMetadata[openaipkg.Name].(*openaipkg.ResponsesReasoningMetadata)
		require.True(t, ok)
		require.NotNil(t, meta.EncryptedContent)
		require.Equal(t, "cipher", *meta.EncryptedContent)
	})

	t.Run("google-gemini index ahead of slice length", func(t *testing.T) {
		t.Parallel()

		choice := reasoningChoice(t, `[
			{"index": 1, "format": "google-gemini", "type": "reasoning.text", "text": "second"}
		]`)

		var content []fantasy.Content
		require.NotPanics(t, func() {
			content = languageModelExtraContent(choice)
		})

		require.Len(t, content, 2)
		rc0, ok := content[0].(fantasy.ReasoningContent)
		require.True(t, ok)
		require.Equal(t, "", rc0.Text)
		// The filler slot must carry non-nil metadata: a later detail can
		// still target it and dereference metadata.Signature/.ToolID.
		require.NotNil(t, rc0.ProviderMetadata)
		rc1, ok := content[1].(fantasy.ReasoningContent)
		require.True(t, ok)
		require.Equal(t, "second", rc1.Text)
	})

	t.Run("google-gemini gap between details, filler metadata non-nil", func(t *testing.T) {
		t.Parallel()

		choice := reasoningChoice(t, `[
			{"index": 0, "format": "google-gemini", "type": "reasoning.text", "text": "first"},
			{"index": 2, "format": "google-gemini", "type": "reasoning.encrypted", "data": "sig", "id": "tool-1"}
		]`)

		var content []fantasy.Content
		require.NotPanics(t, func() {
			content = languageModelExtraContent(choice)
		})

		require.Len(t, content, 3)
		rc0, ok := content[0].(fantasy.ReasoningContent)
		require.True(t, ok)
		require.Equal(t, "first", rc0.Text)

		// index 1 is the filler slot: it was never targeted by a detail,
		// but must still carry non-nil metadata so nothing dereferencing
		// it later panics.
		rc1, ok := content[1].(fantasy.ReasoningContent)
		require.True(t, ok)
		require.NotNil(t, rc1.ProviderMetadata)

		rc2, ok := content[2].(fantasy.ReasoningContent)
		require.True(t, ok)
		meta, ok := rc2.ProviderMetadata[google.Name].(*google.ReasoningMetadata)
		require.True(t, ok)
		require.Equal(t, "sig", meta.Signature)
	})
}

// TestLanguageModelStreamExtra_BareReasoningThenTypedDetailDoesNotPanic pins
// that a stream whose first reasoning chunk carries only the bare
// `reasoning` string (no reasoning_details) does not panic once a later
// chunk supplies a typed detail. The first chunk allocated
// currentReasoningState without its metadata/googleMetadata fields (they are
// only set inside the `len(reasoningData.ReasoningDetails) > 0` branch), and
// a later openai-responses or google-gemini detail then dereferenced the
// still-nil field. Whether the Vercel gateway actually interleaves chunks
// this way in practice is unconfirmed; this pins the code path regardless.
func TestLanguageModelStreamExtra_BareReasoningThenTypedDetailDoesNotPanic(t *testing.T) {
	t.Parallel()

	yield := func(fantasy.StreamPart) bool { return true }

	t.Run("openai-responses detail after bare reasoning start", func(t *testing.T) {
		t.Parallel()

		ctx := map[string]any{}
		require.NotPanics(t, func() {
			var cont bool
			ctx, cont = languageModelStreamExtra(reasoningChunk(t, `"partial thought"`, nil), yield, ctx)
			require.True(t, cont)
			ctx, cont = languageModelStreamExtra(reasoningChunk(t, `""`, `[
				{"index": 0, "format": "openai-responses", "type": "reasoning.summary", "summary": "more"}
			]`), yield, ctx)
			require.True(t, cont)
		})
	})

	t.Run("google-gemini detail after bare reasoning start", func(t *testing.T) {
		t.Parallel()

		ctx := map[string]any{}
		require.NotPanics(t, func() {
			var cont bool
			ctx, cont = languageModelStreamExtra(reasoningChunk(t, `"partial thought"`, nil), yield, ctx)
			require.True(t, cont)
			ctx, cont = languageModelStreamExtra(reasoningChunk(t, `""`, `[
				{"index": 0, "format": "google-gemini", "type": "reasoning.encrypted", "data": "cipher", "id": "tool-1"}
			]`), yield, ctx)
			require.True(t, cont)
		})
	})
}

// reasoningChunk builds an openaisdk.ChatCompletionChunk whose
// Choices[0].Delta.RawJSON() carries the given `reasoning` string and
// `reasoning_details` array, matching the shape languageModelStreamExtra
// parses via json.Unmarshal(choice.Delta.RawJSON(), &ReasoningData{}).
// reasoningDetailsJSON may be nil to omit the field entirely.
func reasoningChunk(t *testing.T, reasoningJSON string, reasoningDetailsJSON any) openaisdk.ChatCompletionChunk {
	t.Helper()

	deltaJSON := fmt.Sprintf(`{"role":"assistant","content":"","reasoning":%s`, reasoningJSON)
	if reasoningDetailsJSON != nil {
		deltaJSON += fmt.Sprintf(`,"reasoning_details":%s`, reasoningDetailsJSON)
	}
	deltaJSON += "}"
	choiceJSON := fmt.Sprintf(`{"index":0,"delta":%s}`, deltaJSON)
	chunkJSON := fmt.Sprintf(`{"id":"chunk-1","created":0,"model":"m","choices":[%s]}`, choiceJSON)

	var chunk openaisdk.ChatCompletionChunk
	require.NoError(t, json.Unmarshal([]byte(chunkJSON), &chunk))
	return chunk
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

// TestLanguagePrepareModelCall_BYOKNestsUnderGateway pins that BYOK
// credentials land at providerOptions.gateway.byok, matching Vercel's
// documented request shape, both when gateway routing options are also set
// (which previously wrote providerOptions.byok directly onto the outer map,
// silently discarding the credentials from the shape Vercel expects) and
// when they are not.
func TestLanguagePrepareModelCall_BYOKNestsUnderGateway(t *testing.T) {
	t.Parallel()

	t.Run("with routing options set", func(t *testing.T) {
		t.Parallel()

		opts := &ProviderOptions{
			ProviderOptions: &GatewayProviderOptions{Order: []string{"anthropic"}},
			BYOK: &BYOKOptions{
				OpenAI: map[string][]BYOKCredential{"default": {{APIKey: "sk-test"}}},
			},
		}
		params := &openaisdk.ChatCompletionNewParams{}
		call := fantasy.Call{ProviderOptions: fantasy.ProviderOptions{Name: opts}}

		_, err := languagePrepareModelCall(nil, params, call)
		require.NoError(t, err)

		extra := params.ExtraFields()
		providerOptions, ok := extra["providerOptions"].(map[string]any)
		require.True(t, ok)
		_, hasTopLevelByok := providerOptions["byok"]
		require.False(t, hasTopLevelByok, "byok must not land directly on providerOptions")

		gateway, ok := providerOptions["gateway"].(map[string]any)
		require.True(t, ok)
		require.NotNil(t, gateway["byok"])
	})

	t.Run("without routing options set", func(t *testing.T) {
		t.Parallel()

		opts := &ProviderOptions{
			BYOK: &BYOKOptions{
				OpenAI: map[string][]BYOKCredential{"default": {{APIKey: "sk-test"}}},
			},
		}
		params := &openaisdk.ChatCompletionNewParams{}
		call := fantasy.Call{ProviderOptions: fantasy.ProviderOptions{Name: opts}}

		_, err := languagePrepareModelCall(nil, params, call)
		require.NoError(t, err)

		extra := params.ExtraFields()
		providerOptions, ok := extra["providerOptions"].(map[string]any)
		require.True(t, ok)
		gateway, ok := providerOptions["gateway"].(map[string]any)
		require.True(t, ok)
		require.NotNil(t, gateway["byok"])
	})
}

// TestLanguageModelToPrompt_MediaToolResultProducesToolMessage pins that a
// media tool result produces a "tool" message carrying its ToolCallID.
// languageModelToPrompt's tool-result switch previously handled only Text
// and Error, so a Media result fell through silently: no message was
// emitted for a tool_calls entry the assistant made, and the next request
// to the API was rejected outright because the tool_calls/tool pairing was
// broken.
func TestLanguageModelToPrompt_MediaToolResultProducesToolMessage(t *testing.T) {
	t.Parallel()

	prompt := fantasy.Prompt{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call-1",
					Output: fantasy.ToolResultOutputContentMedia{
						Data:      "aGVsbG8=",
						MediaType: "image/png",
					},
				},
			},
		},
	}

	messages, warnings := languageModelToPrompt(prompt, "", "some-model")
	require.Empty(t, warnings)
	require.NotEmpty(t, messages)

	var found bool
	for _, m := range messages {
		if m.OfTool != nil && m.OfTool.ToolCallID == "call-1" {
			found = true
		}
	}
	require.True(t, found, "expected a tool message for ToolCallID call-1, got %d messages", len(messages))
}
