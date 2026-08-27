package agent

import (
	"context"
	"time"

	"github.com/rave-soft/sennit/internal/message"
)

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
	// The model this turn was actually dispatched on, not whatever the
	// instance has switched to since — the record written here is what
	// the transcript shows for the cancelled turn.
	model := a.callModel(call)
	assistant, err := a.messages.Create(writeCtx, call.SessionID, message.CreateMessageParams{
		Role:     message.Assistant,
		Parts:    []message.ContentPart{},
		Model:    model.ModelCfg.Model,
		Provider: model.ModelCfg.Provider,
	})
	if err != nil {
		return err
	}
	assistant.AddFinish(message.FinishReasonCanceled, time.Now().Unix(), "User canceled request", "")
	return a.messages.Update(writeCtx, assistant)
}

// Cancel cancels sessionID's active run (if any) and any accepted or
// queued follow-ups. See dispatcher.cancel.
func (a *sessionAgent) Cancel(sessionID string) {
	drops := a.cancel(sessionID)
	a.publishCanceledQueueDrops(drops)
}

// ClearQueue drops sessionID's queued follow-ups without touching the
// active run or any pending cancel mark. See dispatcher.clearQueue.
func (a *sessionAgent) ClearQueue(sessionID string) {
	drops := a.clearQueue(sessionID)
	a.publishCanceledQueueDrops(drops)
}

func (a *sessionAgent) CancelAll() {
	if !a.IsBusy() {
		return
	}
	for _, sessionID := range a.activeSessionIDs() {
		a.Cancel(sessionID)
	}

	// Poll IsBusy on a short tick until every canceled run's cleanup has
	// run, bounded by the same 5s budget as before. There is no
	// broadcast/wait primitive for "a run just finished" (adding one to
	// dispatcher for this single caller would be a bigger change than
	// this polling loop justifies). A 10ms tick keeps this from being a
	// coarse 200ms busy-wait while staying a minimal, local change.
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
