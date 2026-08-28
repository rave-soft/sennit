package common

import (
	"sync"
	"time"

	"github.com/rave-soft/sennit/internal/ui/presentation"
)

// turnTimers tracks per-session elapsed time for the current agent turn.
// A process can host more than one UI instance sharing this package (the
// top-level UI and one embedded per thread — see UI.embedded in
// internal/ui/model), so the timer is keyed by session ID rather than
// kept as a single package-global value.
var turnTimers struct {
	mu    sync.Mutex
	start map[string]time.Time
}

// StartTurn begins tracking elapsed time for a new turn on sessionID.
func StartTurn(sessionID string) {
	turnTimers.mu.Lock()
	defer turnTimers.mu.Unlock()
	if turnTimers.start == nil {
		turnTimers.start = make(map[string]time.Time)
	}
	turnTimers.start[sessionID] = time.Now()
}

// StopTurn stops tracking the turn for sessionID.
func StopTurn(sessionID string) {
	turnTimers.mu.Lock()
	defer turnTimers.mu.Unlock()
	delete(turnTimers.start, sessionID)
}

// Elapsed returns the formatted elapsed time for sessionID's current
// turn. Returns an empty string if no turn is active for it.
func Elapsed(sessionID string) string {
	turnTimers.mu.Lock()
	defer turnTimers.mu.Unlock()
	start, ok := turnTimers.start[sessionID]
	if !ok {
		return ""
	}
	return presentation.FormatElapsed(time.Since(start))
}
