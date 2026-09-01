package model

// asyncOperationState owns correlation and in-flight state for a single async
// operation. It deliberately knows nothing about I/O, commands, dialogs, or UI
// effects; UI orchestrates those concerns around this state machine.
//
// A started operation retains ownership across nonterminal stages. Only a
// matching terminal result consumes it. Stale and unsolicited messages are
// rejected without changing lifecycle state.
type asyncOperationState struct {
	generation uint64
	loading    bool
}

// begin starts an operation and returns its correlation token. A second start
// is rejected while an operation is in flight, without advancing the
// generation.
func (s *asyncOperationState) begin() (generation uint64, started bool) {
	if s.loading {
		return 0, false
	}
	s.loading = true
	s.generation++
	return s.generation, true
}

// owns reports whether generation belongs to the current in-flight operation.
// It is used by nonterminal stages, which retain ownership for the next stage.
func (s *asyncOperationState) owns(generation uint64) bool {
	return s.loading && generation == s.generation
}

// complete consumes the current operation when generation is its token.
func (s *asyncOperationState) complete(generation uint64) bool {
	if !s.owns(generation) {
		return false
	}
	s.loading = false
	return true
}

// isLoading reports whether an operation is in flight.
func (s *asyncOperationState) isLoading() bool {
	return s.loading
}

type modelOperationState struct {
	asyncOperationState
}
