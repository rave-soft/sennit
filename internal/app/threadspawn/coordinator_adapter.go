package threadspawn

import (
	"context"
	"errors"

	"charm.land/fantasy"
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
	inner DelegationCoordinator
}

// ErrInvalidAcceptedRun reports that a reservation was not created by this
// adapter for the concrete agent coordinator it wraps. Forwarding it as nil
// would silently discard the acceptance reservation and break dispatch
// ownership.
var ErrInvalidAcceptedRun = errors.New("threadspawn: incompatible accepted run handle")

// DelegationCoordinator is the subset of agent.Coordinator this seam
// forwards to — the seven methods the six of [thread.Coordinator] map
// onto. It is named here, on the forwarding side, so the port stays a
// two-sided contract: widening what the delegation lifecycle can ask a
// coordinator for takes a deliberate edit here as well as in
// internal/thread, rather than silently becoming possible because the
// whole coordinator was in reach. In particular the lifecycle gets no
// Summarize, no UpdateModels, no RefreshSkills and no CancelAll: those
// are the workspace's own business, driven from internal/app.
type DelegationCoordinator interface {
	agent.CompletionDeliverer
	RunAccepted(ctx context.Context, accept *agent.AcceptedRun, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	BeginAccepted(sessionID string) *agent.AcceptedRun
	Cancel(sessionID string)
	IsSessionBusy(sessionID string) bool
	QueuedPrompts(sessionID string) int
	RegisterDelegationParent(sessionID string, parent agent.DelegationParent)
	SetLiveSession(sessionID string)
}

// The seam narrows the coordinator's contract rather than restating it:
// this fails to compile the moment the two disagree.
var _ DelegationCoordinator = agent.Coordinator(nil)

// NewCoordinatorAdapter wraps c so it satisfies [thread.Coordinator]. A nil
// c (an App that never initialized a coordinator) yields a nil
// [thread.Coordinator], not an adapter wrapping nil: the domain guards a
// nil coordinator before calling it (see lifecycle.cancel), and an
// adapter-with-nil-inner would panic on any call — including Cancel during
// shutdown.
func NewCoordinatorAdapter(c DelegationCoordinator) thread.Coordinator {
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
func (a *coordinatorAdapter) RunAccepted(ctx context.Context, accept *thread.AcceptedRun, sessionID, prompt string, attachments []thread.Attachment) error {
	if accept == nil {
		return ErrInvalidAcceptedRun
	}
	ar, ok := accept.Handle().(*agent.AcceptedRun)
	if !ok || ar == nil {
		return ErrInvalidAcceptedRun
	}
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

func (a *coordinatorAdapter) BeginAccepted(sessionID string) *thread.AcceptedRun {
	return thread.NewAcceptedRun(a.inner.BeginAccepted(sessionID))
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

// SetLiveSession passes the domain's choice straight through: the
// wake rule it feeds lives in the agent, not here.
func (a *coordinatorAdapter) SetLiveSession(sessionID string) {
	a.inner.SetLiveSession(sessionID)
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
		PriorReports:   completion.PriorReports,
		Acknowledge:    completion.Acknowledge,
	})
}

// unwrapCoordinator recovers the parent-delivery slice of the coordinator
// a [thread.Coordinator] wraps, which is all a DelegationParent holds (see
// agent.CompletionDeliverer). Every Coordinator the domain sees in this
// system is a *coordinatorAdapter produced by [NewCoordinatorAdapter] (the
// composition seam is the only place real coordinators are handed to the
// domain), so the assertion is safe; a nil or foreign value degrades to nil,
// matching the domain's "no parent to deliver to" handling.
func unwrapCoordinator(c thread.Coordinator) agent.CompletionDeliverer {
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
