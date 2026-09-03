package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/stretchr/testify/require"
)

// TestRunTurn_CancelBeforeAssistantDrainsQueue is the regression test for
// G17: a cancel landing in the window between dispatchDecision registering
// the active run and PrepareStep creating the turn's first assistant
// message used to return from runTurn without ever calling drainNext - the
// same "we already know foldSteering re-queued its own rollback" early
// return that genuinely does apply to foldSteering's own PrepareStep
// failure, but not to this one.
//
// All of runTurn's own setup before Stream (session.Get, createUserMessage)
// runs on ctx, not genCtx, so a Cancel() landing there does not fail that
// setup - it only cancels genCtx (via the active run's own activeCancel).
// By the time Stream(genCtx, ...) is entered, PrepareStep's
// createStepAssistant tries to persist the assistant message on a context
// derived from the already-canceled genCtx, fails, and t.currentAssistant
// stays nil - landing exactly in handleStreamErrorBeforeAssistant's cancel
// branch. Without draining here, a prompt queued right after the Escape
// sits in the queue with nothing left to pick it up:
// wakeFromInboxIfIdle only looks at the completion inbox, not the message
// queue.
func TestRunTurn_CancelBeforeAssistantDrainsQueue(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	var blockFirstRead sync.Once
	sessions := blockingSessionService{Service: env.sessions, get: func(ctx context.Context, id string) (session.Session, error) {
		block := false
		blockFirstRead.Do(func() { block = true })
		if !block {
			return env.sessions.Get(ctx, id)
		}
		close(readStarted)
		<-releaseRead
		return env.sessions.Get(ctx, id)
	}}

	model := &raceInjectModel{text: "after response"}
	notifyBroker := pubsub.NewBroker[notify.Notification]()
	t.Cleanup(notifyBroker.Shutdown)
	runCompleteBroker := pubsub.NewBroker[notify.RunComplete]()
	t.Cleanup(runCompleteBroker.Shutdown)

	sa := NewSessionAgent(SessionAgentOptions{
		Model:       Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		Sessions:    sessions,
		Messages:    env.messages,
		Notify:      notifyBroker,
		RunComplete: runCompleteBroker,
	}).(*sessionAgent)

	subCtx, subCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer subCancel()
	completions := runCompleteBroker.Subscribe(subCtx)

	runDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, RunID: "main", Prompt: "main"})
		runDone <- runErr
	}()

	select {
	case <-readStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the main turn never reached its blocking session read")
	}

	// Escape: cancel the active run's genCtx while session.Get is still
	// blocked on ctx. Note this session.Get is called with ctx (not
	// genCtx), so it is unaffected and will complete normally once
	// released below.
	sa.Cancel(sess.ID)

	// A prompt typed right after Escape: still busy (the main turn's
	// active entry is not cleared until its own runTurn defer runs, well
	// after this), so it takes the queue branch like any other call
	// arriving while the session is busy.
	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, RunID: "after", Prompt: "after"})
	require.NoError(t, err)
	require.Equal(t, 1, sa.QueuedPrompts(sess.ID), "the post-Escape prompt must be queued behind the canceled turn")

	close(releaseRead)

	select {
	case runErr := <-runDone:
		_ = runErr
	case <-time.After(5 * time.Second):
		t.Fatal("Run never returned")
	}

	seen := map[string]notify.RunComplete{}
	for len(seen) < 2 {
		select {
		case evt := <-completions:
			seen[evt.Payload.RunID] = evt.Payload
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for both RunCompletes, got %v", seen)
		}
	}

	require.True(t, seen["main"].Cancelled, "the main turn's own RunComplete must report Cancelled")
	require.False(t, seen["after"].Cancelled, "the queued follow-up must actually run, not be dropped")
	require.Empty(t, seen["after"].Error)

	require.Equal(t, 0, sa.QueuedPrompts(sess.ID), "the queue must be drained by the hand-off, not left stranded")
	require.False(t, sa.IsSessionBusy(sess.ID))
}
