package threadspawn

import (
	"context"

	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/thread"
)

// coordinatorAdapter adapts a workspace's real agent.Coordinator to the
// thread domain's narrow [thread.Coordinator]. It exists because
// internal/thread must not import internal/agent (whose closure pulls in
// internal/config and internal/db), so the domain declares its own
// Coordinator port and this seam maps between the two spellings.
//
// The mapping is behavior-preserving: RunAccepted forwards the prompt
// (with the attachments only the person's own dispatch carries — see
// thread.Attachment) and discard the agent result the domain never reads;
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
	if onDispatch, ok := thread.SteeringFromContext(ctx); ok {
		// The two spellings of the same three-way decision map one to one.
		// SteerEnqueued is the folding branch for a steering call, since
		// the agent drops such a call's RunID as it enqueues (see
		// agent.SessionAgentCall.Steering).
		ctx = agent.WithSteering(ctx, func(outcome agent.SteerOutcome) {
			if onDispatch == nil {
				return
			}
			switch outcome {
			case agent.SteerEnqueued:
				onDispatch(thread.DispatchFolded)
			case agent.SteerCanceled:
				onDispatch(thread.DispatchCancelled)
			default:
				onDispatch(thread.DispatchRan)
			}
		})
	}
	return ctx
}

// RunAccepted translates the context and forwards directly, discarding
// the agent result the domain never reads and surfacing only the error.
func (a *coordinatorAdapter) RunAccepted(ctx context.Context, accept any, sessionID, prompt string, attachments []thread.Attachment) error {
	ar, _ := accept.(*agent.AcceptedRun)
	_, err := a.inner.RunAccepted(a.translateCtx(ctx), ar, sessionID, prompt, toMessageAttachments(attachments)...)
	return err
}

// toMessageAttachments maps the domain's attachment DTO back to the
// agent's own type, field for field. Only the person's dispatch carries
// any (see thread.Attachment); every other path passes nil and gets nil.
func toMessageAttachments(in []thread.Attachment) []message.Attachment {
	if len(in) == 0 {
		return nil
	}
	out := make([]message.Attachment, 0, len(in))
	for _, a := range in {
		out = append(out, message.Attachment{
			FilePath: a.FilePath,
			FileName: a.FileName,
			MimeType: a.MimeType,
			Content:  a.Content,
		})
	}
	return out
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
