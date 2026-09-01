package model

// modelOperationState owns correlation and in-flight state for model changes.
// It deliberately knows nothing about workspace I/O, commands, dialogs, or UI
// effects; UI orchestrates those concerns around this state machine.
//
// A started operation retains ownership across nonterminal stages (provider or
// model persistence, Copilot import, and agent initialization). Only a
// matching terminal result consumes it. Stale and unsolicited messages are
// rejected without changing lifecycle state.
type modelOperationState struct {
	generation uint64
	loading    bool
}

// begin starts a model operation and returns its correlation token. A second
// start is rejected while an operation is in flight, without advancing the
// generation.
func (s *modelOperationState) begin() (generation uint64, started bool) {
	if s.loading {
		return 0, false
	}
	s.loading = true
	s.generation++
	return s.generation, true
}

// owns reports whether generation belongs to the current in-flight operation.
// It is used by nonterminal stages, which retain ownership for the next stage.
func (s *modelOperationState) owns(generation uint64) bool {
	return s.loading && generation == s.generation
}

// complete consumes the current operation when generation is its token.
func (s *modelOperationState) complete(generation uint64) bool {
	if !s.owns(generation) {
		return false
	}
	s.loading = false
	return true
}

// isLoading reports whether a model operation is in flight.
func (s *modelOperationState) isLoading() bool {
	return s.loading
}
