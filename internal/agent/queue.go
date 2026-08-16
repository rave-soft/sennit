package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// BeginAccepted increments the accept counter for sessionID and returns
// a handle whose Close is the only way to decrement it.
func (a *sessionAgent) BeginAccepted(sessionID string) *AcceptedRun {
	return a.dispatch.BeginAccepted(sessionID)
}

// enqueueCall appends call to the session's message queue. See
// dispatcher.enqueueCall.
func (a *sessionAgent) enqueueCall(call SessionAgentCall) {
	a.dispatch.enqueueCall(call)
}

// drainQueueForStep partitions the session's queued calls for the current
// streaming step. See dispatcher.drainQueueForStep.
func (a *sessionAgent) drainQueueForStep(sessionID string) (fold, canceledWithRunID []SessionAgentCall) {
	return a.dispatch.drainQueueForStep(sessionID)
}

// requeueDrained puts a suffix of a drainQueueForStep fold batch back onto
// the front of the session's queue. See dispatcher.requeueDrained.
func (a *sessionAgent) requeueDrained(sessionID string, remainder []SessionAgentCall) {
	a.dispatch.requeueDrained(sessionID, remainder)
}

// publishCanceledQueueDrops emits a terminal cancelled RunComplete for
// every dropped queued call that carries a RunID. A queued prompt removed
// from the queue without ever running — covered by a pending cancel, or
// cleared by Cancel/ClearQueue — would otherwise leave a caller blocked on
// that RunID: `braid run` ignores live message events and exits only on a
// RunComplete whose RunID matches. Calls without a RunID had no such waiter
// and are dropped silently as before. A detached, bounded context keeps the
// must-deliver publish alive even when the run context that triggered the
// drop is already canceled.
func (a *sessionAgent) publishCanceledQueueDrops(drops []SessionAgentCall) {
	var hasRunID bool
	for _, d := range drops {
		if d.RunID != "" {
			hasRunID = true
			break
		}
	}
	if !hasRunID {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, d := range drops {
		if d.RunID == "" {
			continue
		}
		newCompletionReporter(a, d).publish(ctx, notify.RunComplete{
			SessionID: d.SessionID,
			RunID:     d.RunID,
			Cancelled: true,
		})
	}
}

// clearPendingCancel removes any pending-cancel mark for sessionID. See
// dispatcher.clearPendingCancel.
func (a *sessionAgent) clearPendingCancel(sessionID string) {
	a.dispatch.clearPendingCancel(sessionID)
}

// DeliverTaskCompletion enqueues completion into sessionID's completion
// inbox and, if that leaves the session eligible (idle, not left
// canceled by the user), attempts a continuation turn for it. See
// dispatcher.enqueueCompletion and startContinuation. The attempt does
// not itself drain anything - the completion stays in the inbox until
// whichever turn actually becomes active drains it via PrepareStep, so a
// continuation call that loses its own busy-check race (see run()) drops
// cleanly without losing or duplicating completion's content.
func (a *sessionAgent) DeliverTaskCompletion(ctx context.Context, sessionID string, completion TaskCompletion) {
	if !a.dispatch.enqueueCompletion(sessionID, completion) {
		return
	}
	a.startContinuation(ctx, sessionID, "completion arrived while session was idle")
}

// RegisterDelegationParent records where sessionID should deliver a
// mid-run ask. See dispatcher.RegisterDelegationParent.
func (a *sessionAgent) RegisterDelegationParent(sessionID string, parent DelegationParent) {
	a.dispatch.RegisterDelegationParent(sessionID, parent)
}

// SendToParent delivers a mid-run ask from sessionID to its registered
// parent, riding the exact same delivery path DeliverTaskCompletion
// uses (enqueue, at-most-once, idle-wake) via the registered parent's
// own Coordinator - never this sessionAgent's, since a thread's parent
// may live in an entirely different Coordinator/App. Non-blocking: it
// enqueues and returns without waiting for a reply, exactly like
// DeliverTaskCompletion.
func (a *sessionAgent) SendToParent(ctx context.Context, sessionID, message string) error {
	parent, ok := a.dispatch.delegationParents.Get(sessionID)
	if !ok {
		return fmt.Errorf("agent: session %q has no registered parent to message", sessionID)
	}
	parent.Parent.DeliverTaskCompletion(ctx, parent.ParentSessionID, TaskCompletion{
		DelegationID:   parent.DelegationID,
		Kind:           parent.Kind,
		Name:           parent.Name,
		ChildSessionID: sessionID,
		Depth:          parent.Depth,
		TerminalAt:     time.Now(),
		IsMessage:      true,
		Message:        message,
	})
	return nil
}

// drainCompletionsForStep removes and returns every completion queued for
// sessionID. See dispatcher.drainCompletionsForStep.
func (a *sessionAgent) drainCompletionsForStep(sessionID string) []TaskCompletion {
	return a.dispatch.drainCompletionsForStep(sessionID)
}

// requeueCompletions puts a suffix of a drainCompletionsForStep batch back
// at the front of sessionID's completion inbox. See
// dispatcher.requeueCompletions.
func (a *sessionAgent) requeueCompletions(sessionID string, remainder []TaskCompletion) {
	a.dispatch.requeueCompletions(sessionID, remainder)
}

// publishQueueChanged is wired as dispatch.onQueueChanged (see
// NewSessionAgent) so the dispatcher can signal a queue mutation without
// importing pubsub itself. It publishes a lossy, best-effort
// notify.TypeQueueChanged event; the UI's queued-prompt pill re-probes
// the actual count off it instead of trusting a payload, and still falls
// back to its TTL if this is ever dropped by the broker. Called by
// dispatcher methods only after their per-session dispatch mutex has
// been released, so this may itself take a session's dispatch mutex (a
// subscriber callback could, in principle) without risk of deadlock.
func (a *sessionAgent) publishQueueChanged(sessionID string) {
	if a.notify == nil {
		return
	}
	a.notify.Publish(pubsub.CreatedEvent, notify.Notification{
		SessionID: sessionID,
		Type:      notify.TypeQueueChanged,
	})
}

// persistCanceledTurn writes the user/assistant records for a turn that
// was canceled before (or just as) streaming would have produced them.
// It creates the user message only when it was not already created by an
// earlier createUserMessage call (userMsgCreated) and this is not a
// continuation (whose Prompt is a placeholder, never persisted - see
// SessionAgentCall.Continuation - so there is never a user message to
// create here even on this fallback path), then writes an assistant
// message with FinishReasonCanceled. Both writes use
// context.WithoutCancel(ctx) so workspace shutdown (which cancels the run
// context) can't drop them.
func (a *sessionAgent) persistCanceledTurn(ctx context.Context, call SessionAgentCall, userMsgCreated bool) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if !userMsgCreated && !call.Continuation {
		if _, err := a.createUserMessage(writeCtx, call); err != nil {
			return err
		}
	}
	model := a.model.Get()
	assistant, err := a.messages.Create(writeCtx, call.SessionID, message.CreateMessageParams{
		Role:     message.Assistant,
		Parts:    []message.ContentPart{},
		Model:    model.ModelCfg.Model,
		Provider: model.ModelCfg.Provider,
	})
	if err != nil {
		return err
	}
	assistant.AddFinish(message.FinishReasonCanceled, "User canceled request", "")
	return a.messages.Update(writeCtx, assistant)
}

// Cancel cancels sessionID's active run (if any) and any accepted or
// queued follow-ups. See dispatcher.cancel.
func (a *sessionAgent) Cancel(sessionID string) {
	drops := a.dispatch.cancel(sessionID)
	a.publishCanceledQueueDrops(drops)
}

// ClearQueue drops sessionID's queued follow-ups without touching the
// active run or any pending cancel mark. See dispatcher.clearQueue.
func (a *sessionAgent) ClearQueue(sessionID string) {
	drops := a.dispatch.clearQueue(sessionID)
	a.publishCanceledQueueDrops(drops)
}

func (a *sessionAgent) CancelAll() {
	if !a.IsBusy() {
		return
	}
	for _, sessionID := range a.dispatch.activeSessionIDs() {
		a.Cancel(sessionID)
	}

	// Poll IsBusy on a short tick until every canceled run's cleanup has
	// run, bounded by the same 5s budget as before. activeRequests has no
	// broadcast/wait primitive (csync.Map is a plain generic map used well
	// beyond this file), so a true event-driven wait would mean adding one
	// to a shared type for a single caller. A 10ms tick keeps this from
	// being a coarse 200ms busy-wait while staying a minimal, local change.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for a.IsBusy() {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *sessionAgent) IsBusy() bool {
	return a.dispatch.IsBusy()
}

func (a *sessionAgent) IsSessionBusy(sessionID string) bool {
	return a.dispatch.IsSessionBusy(sessionID)
}

func (a *sessionAgent) QueuedPrompts(sessionID string) int {
	return a.dispatch.QueuedPrompts(sessionID)
}

func (a *sessionAgent) QueuedPromptsList(sessionID string) []string {
	return a.dispatch.QueuedPromptsList(sessionID)
}
