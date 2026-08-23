package common

import (
	"sync"
	"time"

	"github.com/rave-soft/sennit/internal/ui/presentation"
)

// turnTimer tracks the elapsed time for the current agent turn.
var turnTimer struct {
	mu        sync.Mutex
	startTime time.Time
	active    bool
}

// StartTurn begins tracking elapsed time for a new turn.
func StartTurn() {
	turnTimer.mu.Lock()
	defer turnTimer.mu.Unlock()
	turnTimer.startTime = time.Now()
	turnTimer.active = true
}

// StopTurn stops tracking the current turn.
func StopTurn() {
	turnTimer.mu.Lock()
	defer turnTimer.mu.Unlock()
	turnTimer.active = false
}

// Elapsed returns the formatted elapsed time for the current turn.
// Returns empty string if no turn is active.
func Elapsed() string {
	turnTimer.mu.Lock()
	defer turnTimer.mu.Unlock()
	if !turnTimer.active {
		return ""
	}
	return presentation.FormatElapsed(time.Since(turnTimer.startTime))
}
