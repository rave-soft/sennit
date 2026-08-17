package threadspawn

import (
	"context"

	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/thread"
)

// coordinatorAdapter adapts a workspace's real agent.Coordinator to the
// thread domain's narrow [thread.Coordinator]. It exists because
// internal/thread must not import internal/agent (whose closure pulls in
// internal/config and internal/db), so the domain declares its own
// Coordinator port and this seam maps between the two spellings.
//
// The mapping is behavior-preserving: Run/RunAccepted forward the prompt
// with no attachments (the delegation lifecycle never dispatches an
// attached prompt) and discard the agent result the domain never reads;
// the per-run RunID, the agent-dispatch origin tag and the steering tag the
// domain set on its own context keys (see thread.WithRunID /
// thread.WithAgentDispatch / thread.WithSteering) are re-applied to the
// agent's own keys here, so the coordinator's run-id echo, prompt-origin
// persistence and dispatch decision are byte-identical to before the port
// was introduced.
type coordinatorAdapter struct {
	inner agent.Coordinator
}

// NewCoordinatorAdapter wraps c so it satisfies [thread.Coordinator]. A nil
// c (an App that never initialized a coordinator) yields a nil
// [thread.Coordinator], not an adapter wrapping nil: the domain guards a
// nil coordinator before calling it (see lifecycle.cancel), and an
// adapter-with-nil-inner would panic on any call — including Cancel during
// shutdown.
func NewCoordinatorAdapter(c agent.Coordinator) thread.Coordinator {
	if c == nil {
		return nil
	}
	return &coordinatorAdapter{inner: c}
}

// translateCtx copies the domain's per-run RunID and agent-dispatch origin
// tags onto the agent's own context keys, so the real coordinator observes
// them exactly as it did when the domain called it directly.
func (a *coordinatorAdapter) translateCtx(ctx context.Context) context.Context {
	if runID := thread.RunIDFromContext(ctx); runID != "" {
		ctx = agent.WithRunID(ctx, runID)
	}
	if thread.AgentDispatchFromContext(ctx) {
		ctx = agent.WithAgentDispatch(ctx)
	}
	if onFolded, ok := thread.SteeringFromContext(ctx); ok {
		// The domain's hook speaks in "did it fold", the agent's in
		// SteerOutcome; SteerEnqueued is the folding branch for a steering
		// call, since the agent drops such a call's RunID as it enqueues
		// (see agent.SessionAgentCall.Steering). SteerCanceled is
		// unreachable from this path — it needs an Accepted handle, which
		// the lifecycle's steering dispatch does not take — and reads as
		// "no run of ours started", the same as folding, for the one
		// decision the domain makes from this.
		ctx = agent.WithSteering(ctx, func(outcome agent.SteerOutcome) {
			if onFolded != nil {
				onFolded(outcome != agent.SteerRan)
			}
		})
	}
	return ctx
}

// runCoordinator is intentionally absent: Run and RunAccepted below each
// translate the context and forward directly, discarding the agent result
// the domain never reads and surfacing only the error.
func (a *coordinatorAdapter) Run(ctx context.Context, sessionID, prompt string) error {
	_, err := a.inner.Run(a.translateCtx(ctx), sessionID, prompt)
	return err
}

func (a *coordinatorAdapter) RunAccepted(ctx context.Context, accept any, sessionID, prompt string) error {
	ar, _ := accept.(*agent.AcceptedRun)
	_, err := a.inner.RunAccepted(a.translateCtx(ctx), ar, sessionID, prompt)
	return err
}

func (a *coordinatorAdapter) BeginAccepted(sessionID string) any {
	return a.inner.BeginAccepted(sessionID)
}

func (a *coordinatorAdapter) Cancel(sessionID string) {
	a.inner.Cancel(sessionID)
}

// SessionQueue forwards the coordinator's own busy/queue-depth reads. The
// domain port splits them into one call because it only ever wants the
// pair, and reading them together keeps the answer it reports back to a
// sender internally consistent.
func (a *coordinatorAdapter) SessionQueue(sessionID string) (bool, int) {
	return a.inner.IsSessionBusy(sessionID), a.inner.QueuedPrompts(sessionID)
}

func (a *coordinatorAdapter) RegisterDelegationParent(sessionID string, parent thread.DelegationParent) {
	a.inner.RegisterDelegationParent(sessionID, agent.DelegationParent{
		Parent:          unwrapCoordinator(parent.Parent),
		ParentSessionID: parent.ParentSessionID,
		DelegationID:    parent.DelegationID,
		Kind:            parent.Kind,
		Name:            parent.Name,
		Depth:           parent.Depth,
	})
}

func (a *coordinatorAdapter) DeliverTaskCompletion(ctx context.Context, parentSessionID string, completion thread.TaskCompletion) {
	a.inner.DeliverTaskCompletion(ctx, parentSessionID, agent.TaskCompletion{
		DelegationID:   completion.DelegationID,
		Kind:           completion.Kind,
		Name:           completion.Name,
		Goal:           completion.Goal,
		Status:         completion.Status,
		ChildSessionID: completion.ChildSessionID,
		ResultText:     completion.ResultText,
		Error:          completion.Error,
		Depth:          completion.Depth,
		TerminalAt:     completion.TerminalAt,
	})
}

// unwrapCoordinator recovers the concrete agent.Coordinator a
// [thread.Coordinator] wraps. Every Coordinator the domain sees in this
// system is a *coordinatorAdapter produced by [NewCoordinatorAdapter] (the
// composition seam is the only place real coordinators are handed to the
// domain), so the assertion is safe; a nil or foreign value degrades to nil,
// matching the domain's "no parent to deliver to" handling.
func unwrapCoordinator(c thread.Coordinator) agent.Coordinator {
	if a, ok := c.(*coordinatorAdapter); ok {
		return a.inner
	}
	return nil
}

// runCompletionBrokerAdapter adapts a workspace's real
// *pubsub.Broker[notify.RunComplete] to the domain's narrow
// [thread.RunCompletionBroker], converting each event field-for-field so the
// lifecycle observes the same SessionID/RunID/Text/Error/Cancelled it used
// to read from notify.RunComplete.
type runCompletionBrokerAdapter struct {
	inner *pubsub.Broker[notify.RunComplete]
}

// NewRunCompletionBroker wraps b so it satisfies [thread.RunCompletionBroker].
func NewRunCompletionBroker(b *pubsub.Broker[notify.RunComplete]) thread.RunCompletionBroker {
	return &runCompletionBrokerAdapter{inner: b}
}

func (a *runCompletionBrokerAdapter) Subscribe(ctx context.Context) <-chan pubsub.Event[thread.RunComplete] {
	in := a.inner.Subscribe(ctx)
	out := make(chan pubsub.Event[thread.RunComplete])
	go func() {
		defer close(out)
		for {
			// Select on ctx.Done() as well as in: the pump's sole reader is
			// the lifecycle's per-run watcher, and once that watcher cancels
			// (runtime teardown) it stops reading out even though in stays
			// open until the parent broker itself shuts down. Without the
			// done case the pump would block on the unbuffered out send and
			// leak past the delegation's lifetime; with it, cancellation
			// drains it promptly.
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- pubsub.Event[thread.RunComplete]{
					Type: ev.Type,
					Payload: thread.RunComplete{
						SessionID: ev.Payload.SessionID,
						RunID:     ev.Payload.RunID,
						MessageID: ev.Payload.MessageID,
						Text:      ev.Payload.Text,
						Error:     ev.Payload.Error,
						Cancelled: ev.Payload.Cancelled,
					},
				}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

func (a *runCompletionBrokerAdapter) Publish(typ pubsub.EventType, v thread.RunComplete) {
	a.inner.Publish(typ, notify.RunComplete{
		SessionID: v.SessionID,
		RunID:     v.RunID,
		MessageID: v.MessageID,
		Text:      v.Text,
		Error:     v.Error,
		Cancelled: v.Cancelled,
	})
}
