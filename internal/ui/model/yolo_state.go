package model

// yoloState owns the in-flight lifecycle of yolo-mode persistence. It
// deliberately knows nothing about the requested value, workspace I/O,
// commands, or UI effects; UI owns those concerns around this state machine.
type yoloState struct {
	generation uint64
	loading    bool
}

// begin starts a persistence operation and returns its generation token. It
// rejects a second operation until the current result is consumed.
func (s *yoloState) begin() (generation uint64, started bool) {
	if s.loading {
		return 0, false
	}
	s.loading = true
	s.generation++
	return s.generation, true
}

// complete consumes the current operation when generation is its token. Stale
// and unsolicited results are rejected without changing lifecycle state.
func (s *yoloState) complete(generation uint64) bool {
	if !s.loading || generation != s.generation {
		return false
	}
	s.loading = false
	return true
}

// isLoading reports whether a yolo-mode write is in flight.
func (s *yoloState) isLoading() bool {
	return s.loading
}
