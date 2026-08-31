package message

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
)

type MessageRole string

const (
	Assistant MessageRole = "assistant"
	User      MessageRole = "user"
	System    MessageRole = "system"
	Tool      MessageRole = "tool"
)

// MarshalText implements the [encoding.TextMarshaler] interface.
func (r MessageRole) MarshalText() ([]byte, error) {
	return []byte(r), nil
}

// UnmarshalText implements the [encoding.TextUnmarshaler] interface.
// See [MessageRole.MarshalText] for why this lives here.
func (r *MessageRole) UnmarshalText(data []byte) error {
	*r = MessageRole(data)
	return nil
}

// Origin identifies who produced a message: the person actually typed
// it, or another agent dispatched it on their behalf (a delegation's
// initial goal, or a thread_send/task_send follow-up). It is metadata
// about authorship only — it never changes Role or what reaches the
// model; see the toAIMessage conversion in internal/agent.
type Origin string

const (
	// OriginPerson is the default: the person using Sennit typed this
	// message themselves. Every existing row and every message
	// created without an explicit origin is OriginPerson.
	OriginPerson Origin = "person"
	// OriginAgent marks a message another agent produced on the
	// person's behalf, rather than something the person typed.
	OriginAgent Origin = "agent"
)

type FinishReason string

const (
	FinishReasonEndTurn   FinishReason = "end_turn"
	FinishReasonMaxTokens FinishReason = "max_tokens"
	FinishReasonToolUse   FinishReason = "tool_use"
	FinishReasonCanceled  FinishReason = "canceled"
	FinishReasonError     FinishReason = "error"
	// FinishReasonContentFilter is a provider safety/refusal stop
	// (Anthropic stop_reason=refusal, OpenAI content_filter, etc.).
	// The TUI renders this as a REFUSED banner rather than a silent
	// empty turn.
	FinishReasonContentFilter FinishReason = "content_filter"

	// Should never happen
	FinishReasonUnknown FinishReason = "unknown"
)

// MarshalText implements the [encoding.TextMarshaler] interface. See
// [MessageRole.MarshalText] for why this lives here rather than in
// internal/proto.
func (fr FinishReason) MarshalText() ([]byte, error) {
	return []byte(fr), nil
}

// UnmarshalText implements the [encoding.TextUnmarshaler] interface.
func (fr *FinishReason) UnmarshalText(data []byte) error {
	*fr = FinishReason(data)
	return nil
}

type ContentPart interface {
	isPart()
}

type ReasoningContent struct {
	Thinking         string                      `json:"thinking"`
	Signature        string                      `json:"signature"`
	ThoughtSignature string                      `json:"thought_signature"` // Used for google
	ToolID           string                      `json:"tool_id"`           // Used for openrouter google models
	ResponsesData    *ResponsesReasoningMetadata `json:"responses_data"`
	StartedAt        int64                       `json:"started_at,omitempty"`
	FinishedAt       int64                       `json:"finished_at,omitempty"`
}

// responsesReasoningMetadataType tags [ResponsesReasoningMetadata] in its
// JSON envelope. It matches the wire tag
// fantasy/providers/openai.ResponsesReasoningMetadata used before this type
// moved into message, so sessions persisted by older binaries keep decoding.
const responsesReasoningMetadataType = "openai.responses.reasoning_metadata"

// ResponsesReasoningMetadata carries the OpenAI Responses-API bookkeeping
// (reasoning item id, encrypted content, summary) needed to resume a
// reasoning item across turns. It is a local mirror of
// fantasy/providers/openai.ResponsesReasoningMetadata's data shape, kept
// deliberately provider-SDK-free: message is a leaf data model (see
// AGENTS.md's "Proto boundary" section), and internal/agent converts to and
// from the SDK type at the provider boundary.
type ResponsesReasoningMetadata struct {
	ItemID           string   `json:"item_id"`
	EncryptedContent *string  `json:"encrypted_content"`
	Summary          []string `json:"summary"`
}

// MarshalJSON reproduces fantasy's provider-data envelope
// ({"type":...,"data":...}) byte for byte, so a message persisted before
// this type moved out of fantasy/providers/openai still round-trips the
// same way (including the fact that decoding the wrapped bytes back through
// this same envelope shape, mirrored from
// fantasy.MarshalProviderType/UnmarshalProviderType, is a pre-existing
// upstream quirk this move does not change).
func (m ResponsesReasoningMetadata) MarshalJSON() ([]byte, error) {
	type plain ResponsesReasoningMetadata
	data, err := json.Marshal(plain(m))
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}{Type: responsesReasoningMetadataType, Data: data})
}

// UnmarshalJSON is the counterpart to MarshalJSON above: it unwraps the
// envelope that one writes. Reading the envelope's bytes as the plain
// struct instead — what this used to do — decoded every field to its zero
// value, so a reasoning item survived in memory and came back from SQLite
// empty, and a turn resumed after a restart had no item id, no encrypted
// content and no summary to resume from.
//
// The plain form is still accepted, and is what a value written by any
// path that marshalled the struct directly (rather than through the
// envelope) looks like. An envelope is recognised by its own two fields,
// so a plain payload — which has neither — falls through to the direct
// decode.
func (m *ResponsesReasoningMetadata) UnmarshalJSON(data []byte) error {
	type plain ResponsesReasoningMetadata

	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.Type != "" && len(envelope.Data) > 0 {
		var p plain
		if err := json.Unmarshal(envelope.Data, &p); err != nil {
			return err
		}
		*m = ResponsesReasoningMetadata(p)
		return nil
	}

	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*m = ResponsesReasoningMetadata(p)
	return nil
}

func (tc ReasoningContent) String() string {
	return tc.Thinking
}
func (ReasoningContent) isPart() {}

type TextContent struct {
	Text string `json:"text"`
}

func (tc TextContent) String() string {
	return tc.Text
}

func (TextContent) isPart() {}

type ImageURLContent struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

func (iuc ImageURLContent) String() string {
	return iuc.URL
}

func (ImageURLContent) isPart() {}

type BinaryContent struct {
	Path     string
	MIMEType string
	Data     []byte
}

func (bc BinaryContent) String(p catwalk.InferenceProvider) string {
	base64Encoded := base64.StdEncoding.EncodeToString(bc.Data)
	if p == catwalk.InferenceProviderOpenAI {
		return "data:" + bc.MIMEType + ";base64," + base64Encoded
	}
	return base64Encoded
}

func (BinaryContent) isPart() {}

type ToolCall struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Input            string `json:"input"`
	ProviderExecuted bool   `json:"provider_executed"`
	Finished         bool   `json:"finished"`
}

func (ToolCall) isPart() {}

type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Content    string `json:"content"`
	Data       string `json:"data"`
	MIMEType   string `json:"mime_type"`
	Metadata   string `json:"metadata"`
	IsError    bool   `json:"is_error"`
}

func (ToolResult) isPart() {}

type Finish struct {
	Reason  FinishReason `json:"reason"`
	Time    int64        `json:"time"`
	Message string       `json:"message,omitempty"`
	Details string       `json:"details,omitempty"`
}

func (Finish) isPart() {}

// ShellCommand stores a bang-mode shell command and its output as a
// distinct content part so it can be reconstructed on session restore.
type ShellCommand struct {
	Command  string `json:"command"`
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

func (ShellCommand) isPart() {}

// HasShellCommand reports whether the message contains any ShellCommand parts.
func (m *Message) HasShellCommand() bool {
	for _, part := range m.Parts {
		if _, ok := part.(ShellCommand); ok {
			return true
		}
	}
	return false
}

// ShellCommands returns all ShellCommand parts from the message.
func (m *Message) ShellCommands() []ShellCommand {
	var cmds []ShellCommand
	for _, part := range m.Parts {
		if sc, ok := part.(ShellCommand); ok {
			cmds = append(cmds, sc)
		}
	}
	return cmds
}

type Message struct {
	ID               string
	Role             MessageRole
	SessionID        string
	Parts            []ContentPart
	Model            string
	Provider         string
	CreatedAt        int64
	UpdatedAt        int64
	IsSummaryMessage bool
	Origin           Origin

	// SummaryBeforeTokens and SummaryAfterTokens size what a compacted
	// context cost and what it now costs: the tokens the conversation
	// occupied when it was handed to the summarize pass, and the tokens
	// the summary that replaced it occupies. Both are zero on every
	// message that is not a finished summary, and on summaries written
	// before the counts were recorded at all — the collapsed summary row
	// renders without numbers rather than claiming a saving of zero.
	SummaryBeforeTokens int64
	SummaryAfterTokens  int64
}

// SummarySavings reports the before/after token counts of a compacted
// context, and whether they are known at all. A summary written before
// the counts existed, or one whose pass reported no usage, returns
// ok=false — which the UI must render as "no numbers", never as zero.
func (m *Message) SummarySavings() (before, after int64, ok bool) {
	if !m.IsSummaryMessage || m.SummaryBeforeTokens <= 0 {
		return 0, 0, false
	}
	return m.SummaryBeforeTokens, m.SummaryAfterTokens, true
}

func (m *Message) Content() TextContent {
	for _, part := range m.Parts {
		if c, ok := part.(TextContent); ok {
			return c
		}
	}
	return TextContent{}
}

func (m *Message) ReasoningContent() ReasoningContent {
	for _, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			return c
		}
	}
	return ReasoningContent{}
}

func (m *Message) ImageURLContent() []ImageURLContent {
	imageURLContents := make([]ImageURLContent, 0)
	for _, part := range m.Parts {
		if c, ok := part.(ImageURLContent); ok {
			imageURLContents = append(imageURLContents, c)
		}
	}
	return imageURLContents
}

func (m *Message) BinaryContent() []BinaryContent {
	binaryContents := make([]BinaryContent, 0)
	for _, part := range m.Parts {
		if c, ok := part.(BinaryContent); ok {
			binaryContents = append(binaryContents, c)
		}
	}
	return binaryContents
}

func (m *Message) ToolCalls() []ToolCall {
	toolCalls := make([]ToolCall, 0)
	for _, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			toolCalls = append(toolCalls, c)
		}
	}
	return toolCalls
}

func (m *Message) ToolResults() []ToolResult {
	toolResults := make([]ToolResult, 0)
	for _, part := range m.Parts {
		if c, ok := part.(ToolResult); ok {
			toolResults = append(toolResults, c)
		}
	}
	return toolResults
}

func (m *Message) IsFinished() bool {
	for _, part := range m.Parts {
		if _, ok := part.(Finish); ok {
			return true
		}
	}
	return false
}

func (m *Message) FinishPart() *Finish {
	for _, part := range m.Parts {
		if c, ok := part.(Finish); ok {
			return &c
		}
	}
	return nil
}

func (m *Message) FinishReason() FinishReason {
	for _, part := range m.Parts {
		if c, ok := part.(Finish); ok {
			return c.Reason
		}
	}
	return ""
}

// IsErrorLike reports whether the message finished with an error-style
// banner (a real error or a provider safety refusal). The TUI renders
// both through the same banner path.
func (m *Message) IsErrorLike() bool {
	switch m.FinishReason() {
	case FinishReasonError, FinishReasonContentFilter:
		return true
	}
	return false
}

func (m *Message) IsThinking() bool {
	if m.ReasoningContent().Thinking != "" && m.Content().Text == "" && !m.IsFinished() {
		return true
	}
	return false
}

// AppendContent grows the message's text, targeting the same part
// [Message.Content] reads: the first TextContent. That keeps the two in
// sync — a delta written anywhere else would never show up through
// Content, which callers (IsThinking, the TUI) rely on for "what has the
// assistant said so far".
func (m *Message) AppendContent(delta string) {
	for i, part := range m.Parts {
		if c, ok := part.(TextContent); ok {
			m.Parts[i] = TextContent{Text: c.Text + delta}
			return
		}
	}
	m.Parts = append(m.Parts, TextContent{Text: delta})
}

// AppendReasoningContent and the four writers below it all take a copy of
// the existing part and change only the field they own, rather than
// rebuilding one from a hand-written list. A rebuild silently drops
// whatever the list forgets — and it forgot ThoughtSignature, ToolID and
// ResponsesData, which is every piece of provider bookkeeping needed to
// resume a reasoning item on the next turn. turn.go writes the Responses
// metadata and a thought signature and then calls FinishThinking on the
// same part, so the rebuild erased them before the first flush. This is
// the mistake FinishToolCall's own comment warns about.
func (m *Message) AppendReasoningContent(delta string, startedAt int64) {
	found := false
	for i, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			c.Thinking += delta
			m.Parts[i] = c
			found = true
		}
	}
	if !found {
		m.Parts = append(m.Parts, ReasoningContent{
			Thinking:  delta,
			StartedAt: startedAt,
		})
	}
}

func (m *Message) AppendThoughtSignature(signature string, toolCallID string) {
	for i, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			c.ThoughtSignature += signature
			c.ToolID = toolCallID
			m.Parts[i] = c
			return
		}
	}
	m.Parts = append(m.Parts, ReasoningContent{ThoughtSignature: signature})
}

func (m *Message) AppendReasoningSignature(signature string) {
	for i, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			c.Signature += signature
			m.Parts[i] = c
			return
		}
	}
	m.Parts = append(m.Parts, ReasoningContent{Signature: signature})
}

func (m *Message) SetReasoningResponsesData(data *ResponsesReasoningMetadata) {
	for i, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			c.ResponsesData = data
			m.Parts[i] = c
			return
		}
	}
}

func (m *Message) FinishThinking(finishedAt int64) {
	for i, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			if c.FinishedAt == 0 {
				c.FinishedAt = finishedAt
				m.Parts[i] = c
			}
			return
		}
	}
}

func (m *Message) ThinkingDurationSeconds(currentTime int64) int64 {
	reasoning := m.ReasoningContent()
	if reasoning.StartedAt == 0 {
		return 0
	}

	endTime := reasoning.FinishedAt
	if endTime == 0 {
		endTime = currentTime
	}

	return endTime - reasoning.StartedAt
}

func (m *Message) FinishToolCall(toolCallID string) {
	for i, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			if c.ID == toolCallID {
				// Copy and flip the one field, rather than rebuilding from a
				// field list: the list silently dropped ProviderExecuted, and
				// would drop whatever is added to ToolCall next.
				c.Finished = true
				m.Parts[i] = c
				return
			}
		}
	}
}

// AppendToolCallInput grows a streaming tool call's arguments by one
// delta. The call is left unfinished: only the end of the stream decides
// that, and until then Input holds a partial - almost always unparseable -
// fragment of JSON.
func (m *Message) AppendToolCallInput(toolCallID string, inputDelta string) {
	for i, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			if c.ID == toolCallID {
				// Copy and grow the one field rather than rebuilding from
				// a field list, which used to drop ProviderExecuted and
				// would drop whatever is added to ToolCall next - the same
				// reason FinishToolCall above copies.
				c.Input += inputDelta
				m.Parts[i] = c
				return
			}
		}
	}
}

func (m *Message) AddToolCall(tc ToolCall) {
	for i, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			if c.ID == tc.ID {
				m.Parts[i] = tc
				return
			}
		}
	}
	m.Parts = append(m.Parts, tc)
}

func (m *Message) SetToolCalls(tc []ToolCall) {
	// remove any existing tool call part it could have multiple
	parts := make([]ContentPart, 0)
	for _, part := range m.Parts {
		if _, ok := part.(ToolCall); ok {
			continue
		}
		parts = append(parts, part)
	}
	m.Parts = parts
	for _, toolCall := range tc {
		m.Parts = append(m.Parts, toolCall)
	}
}

func (m *Message) AddToolResult(tr ToolResult) {
	m.Parts = append(m.Parts, tr)
}

func (m *Message) SetToolResults(tr []ToolResult) {
	for _, toolResult := range tr {
		m.Parts = append(m.Parts, toolResult)
	}
}

// Clone returns a deep copy of the message with an independent Parts slice.
// This prevents race conditions when the message is modified concurrently.
func (m *Message) Clone() Message {
	clone := *m
	clone.Parts = make([]ContentPart, len(m.Parts))
	copy(clone.Parts, m.Parts)
	return clone
}

// ResetStreamedContent removes all parts that were added during streaming
// (text, reasoning, tool calls, finish) so the message is ready for a
// retry. Non-streamed parts (images, binary attachments, tool results,
// shell commands) are preserved.
func (m *Message) ResetStreamedContent() {
	kept := m.Parts[:0]
	for _, part := range m.Parts {
		switch part.(type) {
		case TextContent, ReasoningContent, ToolCall, Finish:
			// Drop streamed parts.
		default:
			kept = append(kept, part)
		}
	}
	m.Parts = kept
}

func (m *Message) AddFinish(reason FinishReason, finishedAt int64, message, details string) {
	// remove any existing finish part
	for i, part := range m.Parts {
		if _, ok := part.(Finish); ok {
			m.Parts = slices.Delete(m.Parts, i, i+1)
			break
		}
	}
	m.Parts = append(m.Parts, Finish{Reason: reason, Time: finishedAt, Message: message, Details: details})
}

func (m *Message) AddImageURL(url, detail string) {
	m.Parts = append(m.Parts, ImageURLContent{URL: url, Detail: detail})
}

func (m *Message) AddBinary(mimeType string, data []byte) {
	m.Parts = append(m.Parts, BinaryContent{MIMEType: mimeType, Data: data})
}

// attachmentTag picks the element a text attachment is wrapped in. A path
// says "this exists on disk, go read it if you need more"; a bare name like
// paste_3.txt is something the user pasted, which lives nowhere and sends
// the agent chasing a file that was never there. Whatever the tag, the
// content is inlined right below it, so nothing has to be read either way.
func attachmentTag(filePath string) string {
	if filePath == "" || strings.ContainsRune(filePath, '/') || strings.ContainsRune(filePath, '\\') {
		return "file"
	}
	return "pasted_text"
}

func PromptWithTextAttachments(prompt string, attachments []Attachment) string {
	var sb strings.Builder
	sb.WriteString(prompt)
	addedAttachments := false
	for _, content := range attachments {
		if !content.IsText() {
			continue
		}
		if !addedAttachments {
			sb.WriteString("\n<system_info>The items below have been attached by the user — files from disk, and pasted text that exists only here. Consider them in your response.</system_info>\n")
			addedAttachments = true
		}
		tag := attachmentTag(content.FilePath)
		switch {
		case content.FilePath == "":
			sb.WriteString("<file>\n")
		case tag == "file":
			fmt.Fprintf(&sb, "<file path='%s'>\n", content.FilePath)
		default:
			fmt.Fprintf(&sb, "<%s name='%s'>\n", tag, content.FilePath)
		}
		sb.WriteString("\n")
		sb.Write(content.Content)
		fmt.Fprintf(&sb, "\n</%s>\n", tag)
	}
	return sb.String()
}
