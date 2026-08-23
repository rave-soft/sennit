package message

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestResponsesReasoningMetadataMarshalsWithLegacyEnvelope pins the wire
// format ResponsesReasoningMetadata must keep: it used to be
// fantasy/providers/openai.ResponsesReasoningMetadata, whose MarshalJSON
// wrapped the payload in a {"type":...,"data":...} envelope. Sessions
// written before this type moved into message carry that exact envelope
// on disk, so the envelope shape (including its key names and the
// "openai.responses.reasoning_metadata" type tag) must not change.
func TestResponsesReasoningMetadataMarshalsWithLegacyEnvelope(t *testing.T) {
	t.Parallel()

	encrypted := "cipher"
	data, err := json.Marshal(ResponsesReasoningMetadata{
		ItemID:           "item_1",
		EncryptedContent: &encrypted,
		Summary:          []string{"line"},
	})
	require.NoError(t, err)

	require.JSONEq(t,
		`{"type":"openai.responses.reasoning_metadata","data":{"item_id":"item_1","encrypted_content":"cipher","summary":["line"]}}`,
		string(data))
}

// TestReasoningContentResponsesDataSurvivesMarshalParts covers the round
// trip a persisted reasoning item actually takes: ReasoningContent's own
// JSON tag wraps ResponsesData, whose MarshalJSON adds the provider-data
// envelope on top. Decoding used to read those enveloped bytes as the
// plain struct, so every field came back zero — the item id, encrypted
// content and summary needed to resume a reasoning item were gone the
// moment the message left the database.
func TestReasoningContentResponsesDataSurvivesMarshalParts(t *testing.T) {
	t.Parallel()

	encrypted := "cipher"
	parts := []ContentPart{
		ReasoningContent{
			Thinking: "thinking",
			ResponsesData: &ResponsesReasoningMetadata{
				ItemID:           "item_1",
				EncryptedContent: &encrypted,
				Summary:          []string{"line"},
			},
		},
	}

	blob, err := MarshalParts(parts)
	require.NoError(t, err)

	decoded, err := UnmarshalParts(blob, "msg-1")
	require.NoError(t, err)
	require.Len(t, decoded, 1)

	reasoning, ok := decoded[0].(ReasoningContent)
	require.True(t, ok)
	require.NotNil(t, reasoning.ResponsesData)
	require.Equal(t, "item_1", reasoning.ResponsesData.ItemID)
	require.Equal(t, &encrypted, reasoning.ResponsesData.EncryptedContent)
	require.Equal(t, []string{"line"}, reasoning.ResponsesData.Summary)
}

// TestResponsesReasoningMetadataAcceptsThePlainForm keeps the fallback
// honest: a payload written without the envelope still decodes.
func TestResponsesReasoningMetadataAcceptsThePlainForm(t *testing.T) {
	t.Parallel()

	var got ResponsesReasoningMetadata
	require.NoError(t, got.UnmarshalJSON([]byte(`{"item_id":"item_2","summary":["s"]}`)))
	require.Equal(t, "item_2", got.ItemID)
	require.Equal(t, []string{"s"}, got.Summary)
}

func makeTestAttachments(n int, contentSize int) []Attachment {
	attachments := make([]Attachment, n)
	content := []byte(strings.Repeat("x", contentSize))
	for i := range n {
		attachments[i] = Attachment{
			FilePath: fmt.Sprintf("/path/to/file%d.txt", i),
			MimeType: "text/plain",
			Content:  content,
		}
	}
	return attachments
}

func BenchmarkPromptWithTextAttachments(b *testing.B) {
	cases := []struct {
		name        string
		numFiles    int
		contentSize int
	}{
		{"1file_100bytes", 1, 100},
		{"5files_1KB", 5, 1024},
		{"10files_10KB", 10, 10 * 1024},
		{"20files_50KB", 20, 50 * 1024},
	}

	for _, tc := range cases {
		attachments := makeTestAttachments(tc.numFiles, tc.contentSize)
		prompt := "Process these files"

		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = PromptWithTextAttachments(prompt, attachments)
			}
		})
	}
}

func TestResetStreamedContent(t *testing.T) {
	t.Parallel()

	msg := &Message{}
	msg.AddImageURL("https://example.com/img.png", "high")
	msg.AppendContent("partial answer")
	msg.AppendReasoningContent("thinking...")
	msg.AddToolCall(ToolCall{ID: "1", Name: "bash"})
	msg.AddToolResult(ToolResult{ToolCallID: "1", Content: "output"})
	msg.AddFinish(FinishReasonError, "boom", "stream died")

	msg.ResetStreamedContent()

	// Streamed parts are gone.
	require.Empty(t, msg.Content().Text, "text should be cleared")
	require.Empty(t, msg.ReasoningContent().Thinking, "reasoning should be cleared")
	require.Empty(t, msg.ToolCalls(), "tool calls should be cleared")
	require.Nil(t, msg.FinishPart(), "finish should be cleared")

	// Non-streamed parts survive.
	require.Len(t, msg.ImageURLContent(), 1, "image should survive")
	require.Len(t, msg.ToolResults(), 1, "tool results should survive")
}

func TestResetStreamedContentEmpty(t *testing.T) {
	t.Parallel()

	// Reset on an empty message is a no-op and must not panic.
	msg := &Message{}
	msg.ResetStreamedContent()
	require.Empty(t, msg.Parts)
}

// TestPromptWithTextAttachmentsLabelsPastesAsPastes covers the agent going
// off to read paste_3.txt: a pasted buffer has no path, and saying it does
// sends the read tool after a file that never existed.
func TestPromptWithTextAttachmentsLabelsPastesAsPastes(t *testing.T) {
	t.Parallel()

	prompt := PromptWithTextAttachments("look", []Attachment{
		{FilePath: "paste_3.txt", FileName: "paste_3.txt", MimeType: "text/plain", Content: []byte("pasted body")},
	})

	require.Contains(t, prompt, "<pasted_text name='paste_3.txt'>")
	require.Contains(t, prompt, "</pasted_text>")
	require.Contains(t, prompt, "pasted body")
	require.NotContains(t, prompt, "path=", "a paste has no path to advertise")
}

// TestPromptWithTextAttachmentsKeepsPathsForRealFiles is the other half:
// a file attached from disk still tells the agent where it lives.
func TestPromptWithTextAttachmentsKeepsPathsForRealFiles(t *testing.T) {
	t.Parallel()

	prompt := PromptWithTextAttachments("look", []Attachment{
		{FilePath: "/home/u/notes.md", FileName: "notes.md", MimeType: "text/markdown", Content: []byte("body")},
	})

	require.Contains(t, prompt, "<file path='/home/u/notes.md'>")
	require.Contains(t, prompt, "</file>")
}

// TestPromptWithTextAttachmentsWithoutAPathStillWraps guards the third
// case: no path at all, which must not produce mismatched tags.
func TestPromptWithTextAttachmentsWithoutAPathStillWraps(t *testing.T) {
	t.Parallel()

	prompt := PromptWithTextAttachments("look", []Attachment{
		{FileName: "x", MimeType: "text/plain", Content: []byte("body")},
	})

	require.Contains(t, prompt, "<file>")
	require.Contains(t, prompt, "</file>")
}

// TestAppendToolCallInputKeepsTheRestOfTheCall: the append used to rebuild
// the ToolCall from a field list, which silently dropped ProviderExecuted -
// and would drop whatever is added to ToolCall next. A provider-executed
// call demoted to a local one mid-stream would be run again by us.
func TestAppendToolCallInputKeepsTheRestOfTheCall(t *testing.T) {
	t.Parallel()

	msg := &Message{}
	msg.AddToolCall(ToolCall{ID: "1", Name: "web_search", ProviderExecuted: true})

	msg.AppendToolCallInput("1", `{"query":`)
	msg.AppendToolCallInput("1", `"go"}`)
	// A delta for a call this message does not have changes nothing.
	msg.AppendToolCallInput("2", `{"x":1}`)

	calls := msg.ToolCalls()
	require.Len(t, calls, 1)
	require.Equal(t, `{"query":"go"}`, calls[0].Input)
	require.True(t, calls[0].ProviderExecuted, "provider execution must survive the deltas")
	require.False(t, calls[0].Finished, "an append never finishes the call")
}
