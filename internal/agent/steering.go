package agent

import "context"

// steeringContextKey is the unexported context key used to carry a
// steering dispatch's decision hook from the dispatch site down into
// coordinator.run, without forcing a breaking change to the
// Coordinator.Run / RunAccepted signatures. It mirrors runIDContextKey /
// promptOriginContextKey, and like them the coordinator copies the value
// onto the SessionAgentCall it builds (Steering / OnDispatch).
type steeringContextKey struct{}

// WithSteering returns ctx tagged so the prompt dispatched through it is a
// steering follow-up: if the target session is mid-turn the prompt folds
// into that turn's next step instead of queueing behind it, and if the
// session is idle it starts its own turn under the ctx's RunID, exactly
// like an untagged dispatch. See SessionAgentCall.Steering.
//
// onDispatch, when non-nil, is called once with whichever of the two
// happened, from under the session's dispatch mutex and before any turn
// starts streaming - the moment a caller needs if its own bookkeeping
// depends on whether a new run started (see SessionAgentCall.OnDispatch).
// It must not block.
//
// Only the person's own words should be dispatched this way: folding into
// a turn in flight is the interactive "wait, do this instead" path, not a
// way for an agent to interrupt another agent's work.
func WithSteering(ctx context.Context, onDispatch func(SteerOutcome)) context.Context {
	return context.WithValue(ctx, steeringContextKey{}, onDispatch)
}

// SteeringFromContext reports whether [WithSteering] tagged ctx, and
// returns the decision hook it carried (nil if the caller passed none).
// Exported because the coordinator needs to read it; safe to call on any
// context.
func SteeringFromContext(ctx context.Context) (func(SteerOutcome), bool) {
	v, ok := ctx.Value(steeringContextKey{}).(func(SteerOutcome))
	return v, ok
}
