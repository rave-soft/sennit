package message

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

type CreateMessageParams struct {
	Role             MessageRole
	Parts            []ContentPart
	Model            string
	Provider         string
	IsSummaryMessage bool
	Origin           Origin
}

type partType string

const (
	// metaType marks the synthetic version-marker element that
	// MarshalParts always prepends. It is never surfaced as a
	// [ContentPart] to callers.
	metaType         partType = "_meta"
	reasoningType    partType = "reasoning"
	textType         partType = "text"
	imageURLType     partType = "image_url"
	binaryType       partType = "binary"
	toolCallType     partType = "tool_call"
	toolResultType   partType = "tool_result"
	finishType       partType = "finish"
	shellCommandType partType = "shell_command"
)

// partsFormatVersion identifies the shape of the parts blob stored in
// SQLite. It is carried as a synthetic "_meta" wrapper element (see
// formatMeta) rather than a schema change, so old rows without it are
// still valid (implicitly version 0/1 — see below).
//
// Compatibility note: this is a one-time, unavoidable break. A binary
// built before this version marker existed treats ANY unrecognized
// wrapper type — including "_meta" — as fatal, so it cannot read a
// blob written by this or a later binary, exactly as it could not read
// a blob containing any other new part type. Binaries at or after this
// change tolerate unknown wrapper types (see UnmarshalParts) and so
// remain forward-compatible with each other from here on: a future new
// part type, or a future version bump gated on this field, will not
// break reads.
const partsFormatVersion = 1

// formatMeta is the payload of the synthetic "_meta" wrapper element.
// It exists only to satisfy [ContentPart] for marshaling; it never
// appears in a [Message]'s Parts slice.
type formatMeta struct {
	Version int `json:"version"`
}

func (formatMeta) isPart() {}

type partWrapper struct {
	Type partType    `json:"type"`
	Data ContentPart `json:"data"`
}

// MarshalParts marshals content parts to JSON, wrapping each in a
// type-tagged envelope plus a synthetic "_meta" version marker (see
// [partsFormatVersion]). Exported so [github.com/rave-soft/sennit/internal/proto]
// can reuse it for its own JSON encoding, which shares the exact same
// shape as the SQLite storage format.
func MarshalParts(parts []ContentPart) ([]byte, error) {
	wrappedParts := make([]partWrapper, 0, len(parts)+1)
	wrappedParts = append(wrappedParts, partWrapper{
		Type: metaType,
		Data: formatMeta{Version: partsFormatVersion},
	})

	for _, part := range parts {
		var typ partType

		switch part.(type) {
		case ReasoningContent:
			typ = reasoningType
		case TextContent:
			typ = textType
		case ImageURLContent:
			typ = imageURLType
		case BinaryContent:
			typ = binaryType
		case ToolCall:
			typ = toolCallType
		case ToolResult:
			typ = toolResultType
		case Finish:
			typ = finishType
		case ShellCommand:
			typ = shellCommandType
		default:
			return nil, fmt.Errorf("unknown part type: %T", part)
		}

		wrappedParts = append(wrappedParts, partWrapper{
			Type: typ,
			Data: part,
		})
	}
	return json.Marshal(wrappedParts)
}

// UnmarshalParts decodes a parts blob. msgID is included in warning
// logs for unrecognized part types; it is not otherwise used and may
// be empty (e.g. before the message has an ID).
//
// Unknown wrapper types are skipped rather than treated as fatal: a
// session written by a newer binary may contain a part type this
// binary doesn't know about, and failing the whole message would make
// an otherwise-readable session unreadable. A malformed envelope or a
// known type with a payload that fails to unmarshal is still a real
// error (data corruption), so those remain fatal.
func UnmarshalParts(data []byte, msgID string) ([]ContentPart, error) {
	temp := []json.RawMessage{}

	if err := json.Unmarshal(data, &temp); err != nil {
		return nil, err
	}

	parts := make([]ContentPart, 0)

	for _, rawPart := range temp {
		var wrapper struct {
			Type partType        `json:"type"`
			Data json.RawMessage `json:"data"`
		}

		if err := json.Unmarshal(rawPart, &wrapper); err != nil {
			return nil, err
		}

		switch wrapper.Type {
		case metaType:
			// Version marker; nothing to branch on yet since this is
			// the first version. Not appended to parts.
		case reasoningType:
			part := ReasoningContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case textType:
			part := TextContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case imageURLType:
			part := ImageURLContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case binaryType:
			part := BinaryContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case toolCallType:
			part := ToolCall{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case toolResultType:
			part := ToolResult{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case finishType:
			part := Finish{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case shellCommandType:
			part := ShellCommand{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		default:
			slog.Warn("Skipping unrecognized message part type",
				"type", wrapper.Type, "message_id", msgID)
		}
	}

	return parts, nil
}
