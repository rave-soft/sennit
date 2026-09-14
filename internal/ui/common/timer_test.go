package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
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

// TestStartTurnIfIdle covers the two callers it has to serve at once:
// the agent's turn-started event, which fires for every turn including
// the one this client just started itself, and the turn that began
// while nothing here was tracking it.
func TestStartTurnIfIdle(t *testing.T) {
	// Not parallel: turnTimers is package state shared by every test in
	// this package.
	const sessionID = "sess-timer"
	t.Cleanup(func() { StopTurn(sessionID) })

	require.Empty(t, Elapsed(sessionID), "no turn is being tracked yet")

	StartTurnIfIdle(sessionID)
	first := Elapsed(sessionID)
	require.NotEmpty(t, first, "an idle session starts its clock")

	// The second call must not restart the clock: the agent announces
	// every turn, including the one a client already timed, and a reset
	// a few milliseconds in is exactly what this guards against.
	turnTimers.mu.Lock()
	startedAt := turnTimers.start[sessionID]
	turnTimers.mu.Unlock()

	StartTurnIfIdle(sessionID)
	turnTimers.mu.Lock()
	afterSecond := turnTimers.start[sessionID]
	turnTimers.mu.Unlock()
	assert.Equal(t, startedAt, afterSecond, "a running turn's clock must not be restarted")

	// Once the turn ends, the next one starts a fresh clock — this is
	// what makes the agent's event usable for a turn the queue handed to
	// itself after the previous one finished.
	StopTurn(sessionID)
	require.Empty(t, Elapsed(sessionID))
	StartTurnIfIdle(sessionID)
	turnTimers.mu.Lock()
	afterRestart := turnTimers.start[sessionID]
	turnTimers.mu.Unlock()
	assert.True(t, afterRestart.After(startedAt) || afterRestart.Equal(startedAt),
		"a turn following a stopped one gets its own start time")
	require.NotEmpty(t, Elapsed(sessionID))
}
