package message

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// messageFixture is a fully-populated Message: every exported field
// non-zero, and one part of every kind ContentPart has (borrowed from
// roundTripFixtures in roundtrip_test.go, which already keeps that list
// honest against the source). See TestMessageFixture_LeavesNoFieldZero
// and TestMessageFixture_CoversEveryPartKind below for why a fixture
// with a zero field would make this test pass without covering
// anything.
func messageFixture() Message {
	return Message{
		ID:                  "msg-1",
		Role:                Assistant,
		SessionID:           "session-1",
		Parts:               roundTripFixtures,
		Model:               "gpt-5",
		Provider:            "openai",
		CreatedAt:           111,
		UpdatedAt:           222,
		IsSummaryMessage:    true,
		Origin:              OriginAgent,
		SummaryBeforeTokens: 333,
		SummaryAfterTokens:  444,
	}
}

// TestMessage_JSONRoundTrip marshals a fully-populated Message and
// requires the decoded value to equal the original, field for field and
// part for part.
func TestMessage_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	original := messageFixture()

	data, err := json.Marshal(original)
	require.NoError(t, err)

	var decoded Message
	require.NoError(t, json.Unmarshal(data, &decoded))

	require.Equal(t, original, decoded)
}

// TestMessageFixture_LeavesNoFieldZero mirrors
// TestRoundTripFixtures_LeaveNoFieldZero in roundtrip_test.go: a fixture
// field left at its zero value cannot tell a carried field apart from a
// dropped one, so a field added to Message later without updating
// messageFixture must fail loudly here rather than passing silently.
func TestMessageFixture_LeavesNoFieldZero(t *testing.T) {
	t.Parallel()

	m := messageFixture()
	v := reflect.ValueOf(m)
	for i := range v.NumField() {
		field := v.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		require.False(t, v.Field(i).IsZero(),
			"Message.%s is zero in messageFixture, so the round trip does not actually cover it", field.Name)
	}
}

// TestMessageFixture_CoversEveryPartKind requires messageFixture's Parts
// to carry every ContentPart implementation, the same way
// TestRoundTripFixtures_CoverEveryContentPart does for roundTripFixtures
// directly. messageFixture reuses that slice, so this is really a check
// that it keeps doing so.
func TestMessageFixture_CoversEveryPartKind(t *testing.T) {
	t.Parallel()

	m := messageFixture()
	require.ElementsMatch(t, roundTripFixtures, m.Parts)
}

// TestMessage_JSONRoundTrip_NilAndEmptyParts documents the normalization
// UnmarshalJSON applies: both a nil Parts slice and a non-nil empty one
// marshal to the same "parts" blob (MarshalParts always writes at least
// its synthetic "_meta" element) and both decode back to a non-nil empty
// slice, matching what UnmarshalParts already does for the DB path.
func TestMessage_JSONRoundTrip_NilAndEmptyParts(t *testing.T) {
	t.Parallel()

	t.Run("nil parts", func(t *testing.T) {
		t.Parallel()
		original := Message{ID: "msg-nil", Role: User}
		original.Parts = nil

		data, err := json.Marshal(original)
		require.NoError(t, err)

		var decoded Message
		require.NoError(t, json.Unmarshal(data, &decoded))

		require.Equal(t, []ContentPart{}, decoded.Parts)
		original.Parts = []ContentPart{}
		require.Equal(t, original, decoded)
	})

	t.Run("empty parts", func(t *testing.T) {
		t.Parallel()
		original := Message{ID: "msg-empty", Role: User, Parts: []ContentPart{}}

		data, err := json.Marshal(original)
		require.NoError(t, err)

		var decoded Message
		require.NoError(t, json.Unmarshal(data, &decoded))

		require.Equal(t, original, decoded)
	})
}
