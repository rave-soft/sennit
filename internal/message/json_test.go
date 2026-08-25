package message

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnmarshalParts_UnknownTypeSkipped(t *testing.T) {
	t.Parallel()

	raw := `[
		{"type":"text","data":{"text":"before"}},
		{"type":"some_future_type","data":{"whatever":true}},
		{"type":"text","data":{"text":"after"}}
	]`

	parts, err := UnmarshalParts([]byte(raw), "msg-1")
	require.NoError(t, err)
	require.Equal(t, []ContentPart{
		TextContent{Text: "before"},
		TextContent{Text: "after"},
	}, parts)
}

// TestUnmarshalParts_Empty verifies an empty array unmarshals cleanly.
func TestUnmarshalParts_Empty(t *testing.T) {
	t.Parallel()

	parts, err := UnmarshalParts([]byte(`[]`), "msg-1")
	require.NoError(t, err)
	require.Empty(t, parts)
}

// TestUnmarshalParts_OldFormatNoMeta verifies a blob written before the
// "_meta" version marker existed (a plain array of part wrappers) is
// still read correctly with zero data migration.
func TestUnmarshalParts_OldFormatNoMeta(t *testing.T) {
	t.Parallel()

	raw := `[{"type":"text","data":{"text":"hello"}}]`

	parts, err := UnmarshalParts([]byte(raw), "msg-1")
	require.NoError(t, err)
	require.Equal(t, []ContentPart{TextContent{Text: "hello"}}, parts)
}

// TestMarshalUnmarshalParts_RoundTrip verifies a blob produced by the
// new marshalParts round-trips through unmarshalParts, and that the
// synthetic "_meta" element never surfaces as a ContentPart.
func TestMarshalUnmarshalParts_RoundTrip(t *testing.T) {
	t.Parallel()

	original := []ContentPart{
		TextContent{Text: "hi"},
		ToolCall{ID: "call1", Name: "bash", Input: "{}"},
		Finish{Reason: FinishReasonEndTurn, Time: 42},
	}

	data, err := MarshalParts(original)
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

	parts, err := UnmarshalParts(data, "msg-1")
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
		_, err := UnmarshalParts([]byte(`[{"type":"text","data":`), "msg-1")
		require.Error(t, err)
	})

	t.Run("known type with mismatched payload", func(t *testing.T) {
		t.Parallel()
		// finish's Time field is int64; a string payload must fail
		// to unmarshal rather than being silently dropped.
		raw := `[{"type":"finish","data":{"time":"not-a-number"}}]`
		_, err := UnmarshalParts([]byte(raw), "msg-1")
		require.Error(t, err)
	})
}

// TestUpdate_ConcurrentUpdateAndFlushDoesNotLoseData drives a stream of
// updates from one goroutine while another repeatedly calls Flush and
// Get concurrently. This exercises flushOne's channel-based wait for
// an in-flight write (rather than the old sleep-and-poll loop) under
// real contention: neither goroutine should ever observe a lost
// delta, a panic, or a stuck Flush.
