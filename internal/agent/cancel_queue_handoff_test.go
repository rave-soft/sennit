package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// cancelDuringStreamModel drives a turn whose Stream call is genuinely
// slow to observe cancellation - the window this file's test exploits.
// The first (non-title) call blocks until released, then reports
// context.Canceled via a StreamPartTypeError part, exactly as a real
// model does when a canceled context finally surfaces mid-stream.
// Every later call (the queued turn this test hands off to) streams a
// normal, immediate finish so the handoff completes without further
// coordination.
type cancelDuringStreamModel struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (m *cancelDuringStreamModel) Provider() string { return "fake" }
func (m *cancelDuringStreamModel) Model() string    { return "fake-model" }

func (m *cancelDuringStreamModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	n := m.calls.Add(1)
	return func(yield func(fantasy.StreamPart) bool) {
		if n == 1 {
			close(m.entered)
			<-m.release
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: context.Canceled})
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: "done"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *cancelDuringStreamModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not used")
}

func (m *cancelDuringStreamModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not used")
}

func (m *cancelDuringStreamModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not used")
}

// TestRunTurn_CancelHandsOffWhatWasQueuedAfterIt is the regression test
// for DEFECT 2: between dispatcher.cancel (or the caller's own ctx) firing
// and Stream actually observing it, s.active is still this turn's, so a
// prompt submitted in that window takes the busy branch and queues behind
// it instead of being dropped by the cancel mark. run_turn.go's cancel
// branch used to return immediately ("Cancel already clears the queue"),
// stranding that entry until some later turn's foldSteering happened to
// fold it, or - for a RunID-bearing caller - until its own timeout.
//
// The queue is seeded directly rather than raced into existence: the
// "before" entry stands for one whose accept predates the cancel mark
// (dropped, and reported via publishCanceledQueueDrops - the same
// contract drainNext already gives finishTurn's own handoff); the
// "after" entry is accepted with a fresh, higher sequence once the mark
// is in place, matching what a real `thread_send`/`sennit run` caller
// racing the cancel would get from BeginAccepted.
func TestRunTurn_CancelHandsOffWhatWasQueuedAfterIt(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	notifyBroker := pubsub.NewBroker[notify.Notification]()
	t.Cleanup(notifyBroker.Shutdown)
	runCompleteBroker := pubsub.NewBroker[notify.RunComplete]()
	t.Cleanup(runCompleteBroker.Shutdown)

	model := &cancelDuringStreamModel{entered: make(chan struct{}), release: make(chan struct{})}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:       Model{Model: model, CatalogCfg: catwalkModel()},
		Sessions:    env.sessions,
		Messages:    env.messages,
		Notify:      notifyBroker,
		RunComplete: runCompleteBroker,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	subCtx, subCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer subCancel()
	completions := runCompleteBroker.Subscribe(subCtx)

	runDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, RunID: "main", Prompt: "main"})
		runDone <- runErr
	}()

	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the main turn never reached its blocking Stream call")
	}

	// keepAlive stands for a third accepted-but-not-yet-dispatched call
	// elsewhere, still in flight when drainNext runs below. Without it,
	// "before" and "after" closing their own reservations (Close, called
	// by dispatchDecision's busy branch and cancel-on-entry path alike)
	// would drop the ledger's accepted count for this session to zero,
	// and endAccepted clears the cancel mark the moment nothing is left
	// for it to cover (see acceptLedger.endAccepted) - resurrecting
	// "before" by accident before this test ever reaches the code under
	// test.
	keepAlive := sa.BeginAccepted(sess.ID)
	t.Cleanup(keepAlive.Close)

	// "before": an accept whose sequence the cancel mark below will
	// cover. Seeded straight into the queue (mirroring what
	// enqueueLocked would have produced) rather than driven through
	// dispatchDecision, since a call whose Accepted.seq is already
	// covered is caught by the cancel-on-entry fast path before it ever
	// reaches the queue - the queue only ever holds one of these via the
	// same requeue paths drainNext's own doc comment describes.
	before := sa.BeginAccepted(sess.ID)
	sa.setMessageQueueForTest(sess.ID, []SessionAgentCall{
		{SessionID: sess.ID, RunID: "before", Prompt: "before", acceptSeq: before.seq},
	})
	before.Close()
	sa.setCancelMarkForTest(sess.ID, before.seq)

	// "after": accepted once the mark is in place, so its sequence is
	// strictly higher - exactly a `sennit run` caller racing the cancel.
	// The session is still busy (Stream hasn't returned), so this takes
	// the busy branch and queues behind "before".
	after := sa.BeginAccepted(sess.ID)
	require.Greater(t, after.seq, before.seq)
	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, RunID: "after", Prompt: "after", Accepted: after})
	require.NoError(t, err)

	queued, _ := sa.getMessageQueueForTest(sess.ID)
	require.Len(t, queued, 2, "both entries must be queued ahead of the release")

	// Let the blocked Stream call return its (late) cancellation.
	close(model.release)

	select {
	case runErr := <-runDone:
		// The main call's own outcome is a genuine cancellation; the
		// loop discards it in favor of the handed-off "after" turn's
		// result, but its own RunComplete (checked below) still reports
		// Cancelled via runTurn's own deferred publisher - the single
		// choke point this fix does not need to duplicate.
		_ = runErr
	case <-time.After(5 * time.Second):
		t.Fatal("Run never returned")
	}

	seen := map[string]notify.RunComplete{}
	for len(seen) < 3 {
		select {
		case evt := <-completions:
			seen[evt.Payload.RunID] = evt.Payload
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for all three RunCompletes, got %v", seen)
		}
	}

	require.True(t, seen["main"].Cancelled, "the main turn's own RunComplete must report Cancelled")
	require.True(t, seen["before"].Cancelled, "an entry queued before the cancel must still be dropped, not resurrected")
	require.False(t, seen["after"].Cancelled, "an entry queued after the cancel must be handed off and actually run")
	require.Empty(t, seen["after"].Error)

	queued, hasQueued := sa.getMessageQueueForTest(sess.ID)
	require.False(t, hasQueued, "the queue must be drained by the hand-off, not left for a later turn to find")
	require.Empty(t, queued)
	require.False(t, sa.IsSessionBusy(sess.ID))
}
