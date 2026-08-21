package proto

import (
	"encoding/base64"
	"encoding/json"

	"github.com/rave-soft/sennit/internal/message"
)

// Message represents a message in the proto layer.
type Message struct {
	ID        string        `json:"id"`
	Role      MessageRole   `json:"role"`
	SessionID string        `json:"session_id"`
	Parts     []ContentPart `json:"parts"`
	Model     string        `json:"model"`
	Provider  string        `json:"provider"`
	Origin    Origin        `json:"origin,omitempty"`
	CreatedAt int64         `json:"created_at"`
	UpdatedAt int64         `json:"updated_at"`
}

// The message content model (roles, finish reasons, and part types)
// belongs to [message]. Everything below is a type alias over the
// canonical definitions there rather than a copy, so a
// [proto.ContentPart] and a [message.ContentPart] are the same Go
// value with no conversion needed between the UI and core code. This
// mirrors the pattern already used for tool parameter types in
// tools.go.
//
// Note: because [message.ContentPart] declares an unexported isPart()
// method, only types declared in package message can implement it —
// so every concrete part type must be aliased here too, not just the
// interface. That means proto.ReasoningContent and proto.ToolCall now
// carry a couple of provider-bookkeeping fields (thought_signature,
// tool_id, responses_data, provider_executed) that the old
// hand-copied proto structs didn't expose, and proto.ToolResult's
// data/mime_type lose their omitempty. These are additive/cosmetic
// wire changes only — no field present before was removed or
// repurposed, and JSON consumers doing a normal unmarshal are
// unaffected.
type (
	MessageRole      = message.MessageRole
	Origin           = message.Origin
	FinishReason     = message.FinishReason
	ContentPart      = message.ContentPart
	ReasoningContent = message.ReasoningContent
	TextContent      = message.TextContent
	ImageURLContent  = message.ImageURLContent
	BinaryContent    = message.BinaryContent
	ToolCall         = message.ToolCall
	ToolResult       = message.ToolResult
	Finish           = message.Finish
	ShellCommand     = message.ShellCommand
)

const (
	Assistant = message.Assistant
	User      = message.User
	System    = message.System
	Tool      = message.Tool
)

const (
	OriginPerson = message.OriginPerson
	OriginAgent  = message.OriginAgent
)

const (
	FinishReasonEndTurn       = message.FinishReasonEndTurn
	FinishReasonMaxTokens     = message.FinishReasonMaxTokens
	FinishReasonToolUse       = message.FinishReasonToolUse
	FinishReasonCanceled      = message.FinishReasonCanceled
	FinishReasonError         = message.FinishReasonError
	FinishReasonContentFilter = message.FinishReasonContentFilter
	FinishReasonUnknown       = message.FinishReasonUnknown
)

// MarshalParts and UnmarshalParts re-export [message.MarshalParts] and
// [message.UnmarshalParts]. proto's JSON encoding for a message's parts
// is byte-for-byte the same envelope SQLite storage uses (type-tagged
// wrapper plus "_meta" version marker), so proto has no format of its
// own to maintain here.
var (
	MarshalParts   = message.MarshalParts
	UnmarshalParts = message.UnmarshalParts
)

// MarshalJSON implements the [json.Marshaler] interface.
func (m Message) MarshalJSON() ([]byte, error) {
	parts, err := MarshalParts(m.Parts)
	if err != nil {
		return nil, err
	}

	type Alias Message
	return json.Marshal(&struct {
		Parts json.RawMessage `json:"parts"`
		*Alias
	}{
		Parts: json.RawMessage(parts),
		Alias: (*Alias)(&m),
	})
}

// UnmarshalJSON implements the [json.Unmarshaler] interface.
func (m *Message) UnmarshalJSON(data []byte) error {
	type Alias Message
	aux := &struct {
		Parts json.RawMessage `json:"parts"`
		*Alias
	}{
		Alias: (*Alias)(m),
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// aux.Alias embeds m, so m.ID is already populated by the
	// json.Unmarshal above; pass it through for warning-log context.
	parts, err := UnmarshalParts([]byte(aux.Parts), m.ID)
	if err != nil {
		return err
	}

	m.Parts = parts
	return nil
}

// Content returns the first text content part.
func (m *Message) Content() TextContent {
	for _, part := range m.Parts {
		if c, ok := part.(TextContent); ok {
			return c
		}
	}
	return TextContent{}
}

// Note: unlike [message.Message], proto.Message carries no other
// ToolCalls/AddFinish/... accessor and mutator methods beyond
// [Message.Content] above (kept because internal/cmd still reads it
// off an SSE-delivered proto.Message). proto.Message is otherwise a
// read-only wire DTO: the server builds one via messageToProto and
// sends it once, the client immediately converts it back via
// protoToMessage, and all further message mutation happens on the
// canonical [message.Message] on both ends. The rest of the accessor
// and mutator methods were present on the old hand-copied
// proto.Message but were dead code — nothing in the tree ever called
// them — so they aren't carried forward here.

// Attachment represents a file attachment.
type Attachment struct {
	FilePath string `json:"file_path"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	Content  []byte `json:"content"`
}

// MarshalJSON implements the [json.Marshaler] interface.
func (a Attachment) MarshalJSON() ([]byte, error) {
	type Alias Attachment
	return json.Marshal(&struct {
		Content string `json:"content"`
		*Alias
	}{
		Content: base64.StdEncoding.EncodeToString(a.Content),
		Alias:   (*Alias)(&a),
	})
}

// UnmarshalJSON implements the [json.Unmarshaler] interface.
func (a *Attachment) UnmarshalJSON(data []byte) error {
	type Alias Attachment
	aux := &struct {
		Content string `json:"content"`
		*Alias
	}{
		Alias: (*Alias)(a),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	content, err := base64.StdEncoding.DecodeString(aux.Content)
	if err != nil {
		return err
	}
	a.Content = content
	return nil
}
