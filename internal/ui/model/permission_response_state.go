package model

// permissionResponseState owns the current permission request identity and the
// lifecycle of its response. It deliberately knows nothing about permission
// objects, dialogs, workspace I/O, commands, or UI effects; UI orchestrates
// those concerns around this state machine.
//
// Opening a request that is not already visibly open claims a fresh generation
// and resets any in-flight response. A duplicate delivery while that request's
// dialog is open changes nothing. Only a response begun for the current
// request can be completed; stale, mismatched, and unsolicited completions are
// rejected without changing lifecycle state.
type permissionResponseState struct {
	generation uint64
	permission string
	loading    bool
}

// open claims a fresh lifecycle for permission unless it is already represented
// by an open dialog. Reopening an ID after its dialog was dismissed is a new
// lifecycle and therefore receives a new generation.
func (s *permissionResponseState) open(permission string, dialogOpen bool) (generation uint64, opened bool) {
	if dialogOpen && permission == s.permission {
		return s.generation, false
	}
	s.generation++
	s.permission = permission
	s.loading = false
	return s.generation, true
}

// begin starts a response for the current request. Duplicate responses and
// responses for a request replaced since its dialog action was emitted are
// rejected without advancing the generation.
func (s *permissionResponseState) begin(permission string) (generation uint64, started bool) {
	if s.loading || s.generation == 0 || permission != s.permission {
		return 0, false
	}
	s.loading = true
	return s.generation, true
}

// complete consumes the in-flight response only when its request identity and
// generation still match the current lifecycle.
func (s *permissionResponseState) complete(permission string, generation uint64) bool {
	if !s.loading || permission != s.permission || generation != s.generation {
		return false
	}
	s.loading = false
	return true
}

// current returns the current request identity and generation for a response
// command after begin has claimed it.
func (s *permissionResponseState) current() (permission string, generation uint64) {
	return s.permission, s.generation
}
