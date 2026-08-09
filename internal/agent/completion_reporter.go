package agent

import (
	"context"
	"sync"

	"github.com/rave-soft/braid/internal/agent/notify"
)

// completionReporter gives "publish the terminal RunComplete for this
// accepted call exactly once" semantics for a single turn of
// sessionAgent.Run. Run has several branches that can each conclude a
// turn — the cancel-on-entry early return, the streaming defer that
// covers the normal/error/panic-recovery cases, and the tail path that
// publishes explicitly before recursing into a queued prompt — and at
// most one of them may actually emit for a given call.
//
// The tricky case is the tail path: it publishes this turn's
// RunComplete itself and then recurses via `return a.Run(ctx,
// *firstQueued)`, which assigns the recursive call's error into the
// outer function's named return (retErr) before the streaming defer
// runs. Without coordination the defer would fire afterward and
// re-publish using the recursive call's retErr against this turn's
// MessageID/Text — a mixed, racing event for the wrong RunID.
//
// sync.Once is sufficient to fix this without any ordering flag: it
// doesn't matter that the deferred call fires *after* the tail's
// explicit publish and observes the clobbered retErr, because by the
// time it runs, Once has already fired for the tail's publish, so the
// deferred call is simply a no-op.
type completionReporter struct {
	once  sync.Once
	agent *sessionAgent
	call  SessionAgentCall
}

// newCompletionReporter returns a reporter that will deliver at most one
// terminal RunComplete for call.
func newCompletionReporter(a *sessionAgent, call SessionAgentCall) *completionReporter {
	return &completionReporter{agent: a, call: call}
}

// publish emits complete via the agent's publishRunComplete on the first
// call only; later calls are no-ops.
func (r *completionReporter) publish(ctx context.Context, complete notify.RunComplete) {
	r.once.Do(func() {
		r.agent.publishRunComplete(ctx, r.call, complete)
	})
}

// suppress consumes the Once without emitting anything. It is for the
// summarize-continuation tail path: this call's RunID is being re-queued
// under the same identity, so a *different* turn will eventually publish
// its terminal RunComplete, and this reporter must never emit — not now,
// and not later when the streaming defer runs and finds Once already
// spent.
func (r *completionReporter) suppress() {
	r.once.Do(func() {})
}
