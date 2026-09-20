package agent

import (
	"regexp"
	"testing"

	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/stretchr/testify/require"
)

var uuidShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// TestSessionHeaders_CodexCarriesSessionID pins the header the Codex
// backend routes on. Measured live: without it a 20k-token prompt repeated
// unchanged reported zero cached tokens on every attempt; with it, 19840
// of 20035 (see TestLiveCodexPromptCache). A conversation that re-sends
// its whole prefix on every step is what emptied a five-hour allowance in
// minutes, so this is a cost control, not a nicety.
func TestSessionHeaders_CodexCarriesSessionID(t *testing.T) {
	t.Parallel()

	headers := sessionHeaders("session-1", codex.ProviderID)
	require.Regexp(t, uuidShape, headers["session_id"], "the backend's header is spelled as a UUID")
	require.Equal(t, session.HashID("session-1"), headers["x-session-id"])
	require.Equal(t, session.HashID("session-1"), headers["x-session-affinity"])
}

// TestSessionHeaders_SessionIDIsStableAndPerSession: routing is only worth
// anything if every request of one session carries the same value, and two
// sessions carry different ones - they hold different prefixes.
func TestSessionHeaders_SessionIDIsStableAndPerSession(t *testing.T) {
	t.Parallel()

	first := sessionHeaders("session-1", codex.ProviderID)["session_id"]
	again := sessionHeaders("session-1", codex.ProviderID)["session_id"]
	other := sessionHeaders("session-2", codex.ProviderID)["session_id"]

	require.Equal(t, first, again)
	require.NotEqual(t, first, other)
}

// TestSessionHeaders_SessionIDHidesTheSessionID: the value is a digest, not
// the session's own id, which is the rule the two x- headers already follow.
func TestSessionHeaders_SessionIDHidesTheSessionID(t *testing.T) {
	t.Parallel()

	id := "844660d5-e40b-4be1-bf4d-af324a5cfda7"
	require.NotEqual(t, id, sessionHeaders(id, codex.ProviderID)["session_id"])
}

// TestSessionHeaders_OtherProvidersUnchanged: session_id is Codex's own
// header, and no other provider is asked to make sense of it.
func TestSessionHeaders_OtherProvidersUnchanged(t *testing.T) {
	t.Parallel()

	for _, providerID := range []string{"openai", "anthropic", "copilot", ""} {
		headers := sessionHeaders("session-1", providerID)
		require.NotContains(t, headers, "session_id", providerID)
		require.Len(t, headers, 2, providerID)
	}
}
