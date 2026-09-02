package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCreateAgentToolSessionID_RoundTrip pins the "messageID$$toolCallID"
// format: it is persisted as a session's primary key, so a change here
// would orphan existing rows.
func TestCreateAgentToolSessionID_RoundTrip(t *testing.T) {
	sid := CreateAgentToolSessionID("msg-1", "tool-1")
	require.Equal(t, "msg-1$$tool-1", sid)

	msgID, toolID, ok := ParseAgentToolSessionID(sid)
	require.True(t, ok)
	require.Equal(t, "msg-1", msgID)
	require.Equal(t, "tool-1", toolID)
}

// TestParseAgentToolSessionID_RejectsNonAgentToolIDs covers ids that are
// not in the agent-tool format: an ordinary session UUID, and one that
// happens to contain more than one "$$" separator.
func TestParseAgentToolSessionID_RejectsNonAgentToolIDs(t *testing.T) {
	for _, id := range []string{
		"plain-session-id",
		"",
		"a$$b$$c",
	} {
		_, _, ok := ParseAgentToolSessionID(id)
		require.Falsef(t, ok, "ParseAgentToolSessionID(%q) reported ok=true, want false", id)
	}
}
