package model

// transparencyState owns the in-flight lifecycle of transparency persistence.
// It deliberately knows nothing about the requested value, configuration I/O,
// commands, or UI effects; UI owns those concerns around this state machine.
type transparencyState struct {
	generation uint64
	loading    bool
}

// begin starts a persistence operation and returns its generation token. It
// rejects a second operation until the current result is consumed.
func (s *transparencyState) begin() (generation uint64, started bool) {
	if s.loading {
		return 0, false
	}
	s.loading = true
	s.generation++
	return s.generation, true
}

// complete consumes the current operation when generation is its token. Stale
// and unsolicited results are rejected without changing lifecycle state.
func (s *transparencyState) complete(generation uint64) bool {
	if !s.loading || generation != s.generation {
		return false
	}
	s.loading = false
	return true
}

// isLoading reports whether a transparency write is in flight.
func (s *transparencyState) isLoading() bool {
	return s.loading
}
