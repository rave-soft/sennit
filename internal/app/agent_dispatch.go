package app

import (
	"context"
	"errors"
	"sync"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// ErrDispatcherClosed is returned by [AgentDispatcher.Send] when the
// dispatcher's owner has begun tearing down and is no longer accepting
// new runs.
var ErrDispatcherClosed = errors.New("agent dispatcher closed")

// ErrCoordinatorNotInitialized is returned by [AgentDispatcher.Send]
// when its coordinator getter yields nil, i.e. the owner has not
// initialized an agent.Coordinator yet.
var ErrCoordinatorNotInitialized = errors.New("agent coordinator not initialized")

// AcceptedRunner is the subset of agent.Coordinator's API the
// AgentDispatcher needs: reserve acceptance for a session, then execute
// the run that reservation stands for. Declared here, on the consumer
// side, so the dispatcher cannot reach for the rest of the coordinator —
// it must not cancel, summarize, inspect the queue or swap models, since
// every one of those is somebody else's decision arriving on its own
// path, and a dispatcher that could make them would be a second place
// turn policy lives.
type AcceptedRunner interface {
	BeginAccepted(sessionID string) *agent.AcceptedRun
	RunAccepted(ctx context.Context, accept *agent.AcceptedRun, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
}

// The port is a narrowing of the coordinator's contract, never a
// divergent one: this fails to compile the moment the two disagree.
var _ AcceptedRunner = agent.Coordinator(nil)

// AgentDispatcher accepts prompts for an [AcceptedRunner] and
// dispatches each accepted run on its own goroutine, bound to a fixed
// context supplied at construction. That binding is what makes a run's
// lifetime independent of whichever caller requested it: the owner
// (e.g. a workspace) controls when runs are cut off, not the request
// that triggered one.
type AgentDispatcher struct {
	ctx context.Context
	// coordinator resolves the current coordinator on every call
	// instead of capturing one at construction time. The dispatcher is
	// meant to be built once for the whole life of its owner (e.g. a
	// workspace), but the coordinator it fans out to can be created —
	// or replaced — after that: BeginAccepted/RunAccepted always need
	// today's value, not the one that existed at construction.
	coordinator    func() AcceptedRunner
	notifications  *pubsub.Broker[notify.Notification]
	runCompletions *pubsub.Broker[notify.RunComplete]

	// mu guards closing and gates dispatch of new runs. closing is set
	// by MarkClosing so no new runs are accepted once teardown has
	// begun. wg tracks dispatched run goroutines so Wait can block
	// until they return before the owner tears down further state.
	mu      sync.Mutex
	closing bool
	wg      sync.WaitGroup
}

// NewAgentDispatcher builds an AgentDispatcher that binds dispatched
// runs to ctx and reports through notifications and runCompletions.
// ctx should be a long-lived context owned by the caller (not
// context.Background()) so the caller retains control over when
// in-flight runs are cut off.
//
// coordinator is called fresh on every Send/run rather than resolved
// once, because the dispatcher is constructed alongside its owner
// (e.g. in CreateWorkspace) before the coordinator necessarily exists,
// and the owner may (re)initialize its coordinator afterwards. notifications
// and runCompletions are assumed stable for the owner's lifetime.
func NewAgentDispatcher(ctx context.Context, coordinator func() AcceptedRunner, notifications *pubsub.Broker[notify.Notification], runCompletions *pubsub.Broker[notify.RunComplete]) *AgentDispatcher {
	return &AgentDispatcher{
		ctx:            ctx,
		coordinator:    coordinator,
		notifications:  notifications,
		runCompletions: runCompletions,
	}
}

// Send validates and accepts a prompt for the coordinator, then
// dispatches the run on a goroutine bound to the dispatcher's context
// and returns immediately. It does not wait for the LLM turn to
// complete: the run's lifetime is owned by the dispatcher, not by the
// caller. Errors from the dispatched run reach observers through the
// agent event channels (a notify.TypeAgentError notification), not
// through this return value.
//
// Send returns synchronously when the request cannot be accepted: the
// structural validation errors from agent.ValidateCall
// (agent.ErrEmptyPrompt, agent.ErrSessionMissing) when the prompt or
// session is missing, ErrCoordinatorNotInitialized if the coordinator
// getter yields nil, and ErrDispatcherClosed if the dispatcher's owner
// is being torn down.
func (d *AgentDispatcher) Send(sessionID, runID, prompt string, attachments []message.Attachment) error {
	if err := agent.ValidateCall(agent.SessionAgentCall{
		SessionID:   sessionID,
		Prompt:      prompt,
		Attachments: attachments,
	}); err != nil {
		return err
	}

	coordinator := d.coordinator()
	if coordinator == nil {
		return ErrCoordinatorNotInitialized
	}

	accept := coordinator.BeginAccepted(sessionID)

	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		accept.Close()
		return ErrDispatcherClosed
	}
	d.wg.Add(1)
	d.mu.Unlock()

	go d.run(coordinator, sessionID, runID, prompt, attachments, accept)
	return nil
}

// run executes an accepted agent run. It owns the accept reservation
// (releasing it on return) and the wg ticket added by Send. The run is
// bound to the dispatcher's context so its lifetime is independent of
// any client's request.
//
// On a non-cancel error it surfaces the failure to observers via a
// notify.TypeAgentError notification (lossy, best-effort). That alone
// is not a reliable terminal signal: the agent-event fan-in uses lossy
// subscribers, so a `sennit run` caller blocking on its RunID could
// hang if the event is dropped. To guarantee termination, when runID
// is non-empty and the coordinator did not already publish the run's
// authoritative terminal RunComplete (e.g. the error was returned
// before sessionAgent.Run executed, such as a readyWg or UpdateModels
// failure), run emits an errored RunComplete on the must-deliver
// runCompletions broker so the waiter observes a deterministic
// terminal event. context.Canceled is expected (sessionAgent.Run
// already publishes the cancelled terminal marker) and produces no
// error terminal event.
//
// When runID is non-empty it is attached to the context via
// agent.WithRunID so the coordinator can stamp the terminal
// notify.RunComplete event with that correlator. A run-complete marker
// is also attached so the coordinator can report whether it published
// the terminal event, letting run avoid a duplicate fallback.
func (d *AgentDispatcher) run(coordinator AcceptedRunner, sessionID, runID, prompt string, attachments []message.Attachment, accept *agent.AcceptedRun) {
	defer d.wg.Done()
	defer accept.Close()

	ctx := d.ctx
	if runID != "" {
		ctx = agent.WithRunID(ctx, runID)
	}
	ctx = agent.WithRunCompleteMarker(ctx)

	_, err := coordinator.RunAccepted(ctx, accept, sessionID, prompt, attachments...)
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}

	d.notifications.Publish(pubsub.CreatedEvent, notify.Notification{
		SessionID: sessionID,
		RunID:     runID,
		Type:      notify.TypeAgentError,
		Message:   err.Error(),
	})

	// Reliable terminal fallback. Only needed when a RunID waiter
	// exists and the coordinator has not already emitted the run's
	// terminal RunComplete; otherwise this would be a duplicate.
	if runID == "" || agent.RunCompletePublished(ctx) {
		return
	}
	if d.runCompletions != nil {
		d.runCompletions.PublishMustDeliver(ctx, pubsub.UpdatedEvent, notify.RunComplete{
			SessionID: sessionID,
			RunID:     runID,
			Error:     err.Error(),
		})
	}
}

// MarkClosing latches the decision to stop accepting new runs. Once
// set, every subsequent Send is refused with ErrDispatcherClosed. It
// is split from Wait so the owner can flip the gate before cancelling
// the context runs are bound to (closing the gate after cancellation
// would let a run slip past the check and start against an
// already-cancelled context).
func (d *AgentDispatcher) MarkClosing() {
	d.mu.Lock()
	d.closing = true
	d.mu.Unlock()
}

// Wait blocks until every dispatched run has returned. Callers
// typically call MarkClosing, cancel the bound context, and cancel any
// active coordinator work before calling Wait, so in-flight runs
// observe cancellation instead of running to completion.
func (d *AgentDispatcher) Wait() {
	d.wg.Wait()
}
