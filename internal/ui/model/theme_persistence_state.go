package model

// themePersistenceState owns correlation for theme-persistence operations. It
// deliberately knows nothing about palette IDs, configuration I/O, commands,
// or UI effects; UI owns those concerns around this state machine.
//
// Each begin supersedes any earlier operation. Consequently, only the most
// recent persistence result can be completed: an older write cannot restore
// or report over a newer optimistic selection.
type themePersistenceState struct {
	generation uint64
	pending    bool
}

// begin starts a theme-persistence operation and returns its unique generation
// token. Unlike settings with a single in-flight operation, a newer theme
// selection supersedes an earlier write so the UI can retain its optimistic
// selection while both writes are in flight.
func (s *themePersistenceState) begin() uint64 {
	s.generation++
	s.pending = true
	return s.generation
}

// complete consumes the latest operation when generation is its token. Stale,
// duplicate, and unsolicited results are rejected without changing lifecycle
// state.
func (s *themePersistenceState) complete(generation uint64) bool {
	if !s.pending || generation != s.generation {
		return false
	}
	s.pending = false
	return true
}
