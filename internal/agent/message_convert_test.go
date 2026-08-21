package agent

import (
	"encoding/base64"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

func TestToAIMessage_CorruptedMediaData(t *testing.T) {
	t.Parallel()

	msg := &message.Message{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "call_123",
				Name:       "screenshot",
				Content:    "Loaded image/png content",
				Data:       "abc\x80def",
				MIMEType:   "image/png",
			},
		},
	}

	messages := toAIMessage(msg)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].Content, 1)

	part, ok := messages[0].Content[0].(fantasy.ToolResultPart)
	require.True(t, ok)

	require.Equal(t, "call_123", part.ToolCallID)

	textContent, ok := part.Output.(fantasy.ToolResultOutputContentText)
	require.True(t, ok, "corrupted media should be downgraded to text")
	require.Equal(t, mediaLoadFailedPlaceholder, textContent.Text)
}

func TestToAIMessage_ValidMediaData(t *testing.T) {
	t.Parallel()

	validBase64 := base64.StdEncoding.EncodeToString([]byte{0x89, 0x50, 0x4E, 0x47})

	msg := &message.Message{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "call_456",
				Name:       "screenshot",
				Content:    "Loaded image/png content",
				Data:       validBase64,
				MIMEType:   "image/png",
			},
		},
	}

	messages := toAIMessage(msg)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].Content, 1)

	part, ok := messages[0].Content[0].(fantasy.ToolResultPart)
	require.True(t, ok)

	require.Equal(t, "call_456", part.ToolCallID)

	mediaContent, ok := part.Output.(fantasy.ToolResultOutputContentMedia)
	require.True(t, ok, "valid media should remain as media")
	require.Equal(t, validBase64, mediaContent.Data)
	require.Equal(t, "image/png", mediaContent.MediaType)
}

func TestToAIMessage_ASCIIButInvalidBase64(t *testing.T) {
	t.Parallel()

	msg := &message.Message{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "call_789",
				Name:       "screenshot",
				Content:    "Loaded image/png content",
				Data:       "not-valid-base64!!!",
				MIMEType:   "image/png",
			},
		},
	}

	messages := toAIMessage(msg)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].Content, 1)

	part, ok := messages[0].Content[0].(fantasy.ToolResultPart)
	require.True(t, ok)

	require.Equal(t, "call_789", part.ToolCallID)

	textContent, ok := part.Output.(fantasy.ToolResultOutputContentText)
	require.True(t, ok, "ASCII but invalid base64 should be downgraded to text")
	require.Equal(t, mediaLoadFailedPlaceholder, textContent.Text)
}

// TestToAIMessage_SystemRoleIsNotSentToTheModel pins the property the
// thread-removal note in the chat history depends on: a system-role
// message is a record for the person reading the transcript, and must
// contribute nothing to the prompt. The model already learns that a
// delegation finished through the completion inbox; a second telling, in
// a different voice, would invite it to report the same event twice.
func TestToAIMessage_SystemRoleIsNotSentToTheModel(t *testing.T) {
	t.Parallel()

	m := message.Message{
		Role:  message.System,
		Parts: []message.ContentPart{message.TextContent{Text: `Thread "tidy-up" merged into main and removed.`}},
	}
	require.Empty(t, toAIMessage(&m), "a system-role message must not reach the provider")
}

// TestToAIMessage_ResponsesDataConvertsToProviderType proves the OpenAI
// Responses-API reasoning bookkeeping stored on message.ReasoningContent
// (message.ResponsesReasoningMetadata, a provider-SDK-free mirror - see its
// doc comment) reaches the provider request as the real
// fantasy/providers/openai.ResponsesReasoningMetadata the SDK expects, with
// every field carried across.
func TestToAIMessage_ResponsesDataConvertsToProviderType(t *testing.T) {
	t.Parallel()

	encrypted := "cipher"
	m := &message.Message{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{
				Thinking: "thinking it through",
				ResponsesData: &message.ResponsesReasoningMetadata{
					ItemID:           "item_1",
					EncryptedContent: &encrypted,
					Summary:          []string{"summary line"},
				},
			},
		},
	}

	messages := toAIMessage(m)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].Content, 1)

	reasoningPart, ok := messages[0].Content[0].(fantasy.ReasoningPart)
	require.True(t, ok)

	data, ok := reasoningPart.ProviderOptions[openai.Name].(*openai.ResponsesReasoningMetadata)
	require.True(t, ok)
	require.Equal(t, "item_1", data.ItemID)
	require.Equal(t, &encrypted, data.EncryptedContent)
	require.Equal(t, []string{"summary line"}, data.Summary)
}
