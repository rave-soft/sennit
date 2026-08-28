package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTurnTimerIsPerSession proves that two sessions running turns at the
// same time don't share a clock: stopping one session's turn must not
// blank out another session's elapsed time. On the old package-global
// timer this fails, since StopTurn("b") would clear the single shared
// state that StartTurn("a") had set.
func TestTurnTimerIsPerSession(t *testing.T) {
	StartTurn("a")
	StartTurn("b")
	StopTurn("b")

	require.NotEmpty(t, Elapsed("a"))
	require.Empty(t, Elapsed("b"))

	StopTurn("a")
}

// TestTurnTimerStopReleasesState confirms StopTurn removes its entry
// rather than leaving it behind, so the map does not grow by one entry
// per session for the life of the process.
func TestTurnTimerStopReleasesState(t *testing.T) {
	turnTimers.mu.Lock()
	before := len(turnTimers.start)
	turnTimers.mu.Unlock()

	StartTurn("release-me")
	StopTurn("release-me")

	turnTimers.mu.Lock()
	after := len(turnTimers.start)
	turnTimers.mu.Unlock()

	require.Equal(t, before, after)
}

// TestTurnTimerUnknownSessionIsEmpty confirms Elapsed on a session that
// never started (or has already stopped) reports no active turn.
func TestTurnTimerUnknownSessionIsEmpty(t *testing.T) {
	require.Empty(t, Elapsed("never-started"))
}
