package proto_test

import (
	"encoding/json"
	"testing"

	"github.com/rave-soft/braid/internal/proto"
	"github.com/stretchr/testify/require"
)

// TestUnmarshalParts_UnknownTypeSkipped verifies that an unrecognized
// part type in the middle of the array is dropped rather than failing
// the whole message, so a session written by a newer binary stays
// readable on this one.
func TestUnmarshalParts_UnknownTypeSkipped(t *testing.T) {
	t.Parallel()

	raw := `[
		{"type":"text","data":{"text":"before"}},
		{"type":"some_future_type","data":{"whatever":true}},
		{"type":"text","data":{"text":"after"}}
	]`

	parts, err := proto.UnmarshalParts([]byte(raw), "msg-1")
	require.NoError(t, err)
	require.Equal(t, []proto.ContentPart{
		proto.TextContent{Text: "before"},
		proto.TextContent{Text: "after"},
	}, parts)
}

// TestUnmarshalParts_Empty verifies an empty array unmarshals cleanly.
func TestUnmarshalParts_Empty(t *testing.T) {
	t.Parallel()

	parts, err := proto.UnmarshalParts([]byte(`[]`), "msg-1")
	require.NoError(t, err)
	require.Empty(t, parts)
}

// TestUnmarshalParts_OldFormatNoMeta verifies a blob written before the
// "_meta" version marker existed (a plain array of part wrappers) is
// still read correctly with zero data migration.
func TestUnmarshalParts_OldFormatNoMeta(t *testing.T) {
	t.Parallel()

	raw := `[{"type":"text","data":{"text":"hello"}}]`

	parts, err := proto.UnmarshalParts([]byte(raw), "msg-1")
	require.NoError(t, err)
	require.Equal(t, []proto.ContentPart{proto.TextContent{Text: "hello"}}, parts)
}

// TestMarshalUnmarshalParts_RoundTrip verifies a blob produced by the
// new MarshalParts round-trips through UnmarshalParts, and that the
// synthetic "_meta" element never surfaces as a ContentPart.
func TestMarshalUnmarshalParts_RoundTrip(t *testing.T) {
	t.Parallel()

	original := []proto.ContentPart{
		proto.TextContent{Text: "hi"},
		proto.ToolCall{ID: "call1", Name: "bash", Input: "{}"},
		proto.Finish{Reason: proto.FinishReasonEndTurn, Time: 42},
	}

	data, err := proto.MarshalParts(original)
	require.NoError(t, err)

	// The wrapper array must carry the "_meta" marker as element 0.
	var raw []json.RawMessage
	require.NoError(t, json.Unmarshal(data, &raw))
	require.Len(t, raw, len(original)+1)
	var firstWrapper struct {
		Type string `json:"type"`
	}
	require.NoError(t, json.Unmarshal(raw[0], &firstWrapper))
	require.Equal(t, "_meta", firstWrapper.Type)

	parts, err := proto.UnmarshalParts(data, "msg-1")
	require.NoError(t, err)
	require.Equal(t, original, parts)
}

// TestUnmarshalParts_MalformedNotSwallowed verifies that real
// corruption (broken JSON, or a known type whose payload doesn't
// match its struct) still returns an error and is not accidentally
// treated as an unrecognized-type skip.
func TestUnmarshalParts_MalformedNotSwallowed(t *testing.T) {
	t.Parallel()

	t.Run("broken JSON syntax", func(t *testing.T) {
		t.Parallel()
		_, err := proto.UnmarshalParts([]byte(`[{"type":"text","data":`), "msg-1")
		require.Error(t, err)
	})

	t.Run("known type with mismatched payload", func(t *testing.T) {
		t.Parallel()
		// finish's Time field is int64; a string payload must fail
		// to unmarshal rather than being silently dropped.
		raw := `[{"type":"finish","data":{"time":"not-a-number"}}]`
		_, err := proto.UnmarshalParts([]byte(raw), "msg-1")
		require.Error(t, err)
	})
}

// TestMessage_JSONRoundTrip verifies the proto.Message JSON
// marshal/unmarshal path (which routes through MarshalParts /
// UnmarshalParts) survives round-tripping with the new format.
func TestMessage_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	msg := proto.Message{
		ID:   "msg-1",
		Role: proto.Assistant,
		Parts: []proto.ContentPart{
			proto.TextContent{Text: "hello"},
		},
	}

	data, err := json.Marshal(msg)
	require.NoError(t, err)

	var got proto.Message
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, msg.Parts, got.Parts)
	require.Equal(t, msg.ID, got.ID)
}
