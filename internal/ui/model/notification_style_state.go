package model

// notificationStyleState owns the in-flight lifecycle of notification-style
// persistence. It deliberately knows nothing about the selected style,
// configuration I/O, commands, or UI effects; UI owns those concerns around
// this state machine.
type notificationStyleState struct {
	generation uint64
	loading    bool
}

// begin starts a persistence operation and returns its generation token. It
// rejects a second operation until the current result is consumed.
func (s *notificationStyleState) begin() (generation uint64, started bool) {
	if s.loading {
		return 0, false
	}
	s.loading = true
	s.generation++
	return s.generation, true
}

// complete consumes the current operation when generation is its token. Stale
// and unsolicited results are rejected without changing lifecycle state.
func (s *notificationStyleState) complete(generation uint64) bool {
	if !s.loading || generation != s.generation {
		return false
	}
	s.loading = false
	return true
}

// isLoading reports whether a notification-style write is in flight.
func (s *notificationStyleState) isLoading() bool {
	return s.loading
}
