package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// failingStreamModel fails its first (non-title) Stream call outright,
// simulating a genuine provider/network error rather than a cancellation.
// Any later call (the queued follow-up draining after teardown) succeeds
// normally.
type failingStreamModel struct {
	calls atomic.Int32
}

func (*failingStreamModel) Provider() string { return "fake" }
func (*failingStreamModel) Model() string    { return "fake-model" }

func (m *failingStreamModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *failingStreamModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	if m.calls.Add(1) == 1 {
		return nil, errors.New("stream boom")
	}
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "done"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *failingStreamModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *failingStreamModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// TestRunTurn_StreamErrorStillDrainsQueue is the regression test for the
// Stream-error path returning before finishTurn's release/notify/drain
// tail: a non-cancel Stream error used to return straight out of runTurn,
// so the only drainNext call in the turn path (inside finishTurn) was never
// reached. A prompt already queued behind the failed turn - including one
// carrying its own RunID, like a `sennit run` caller - sat in the queue
// with no hand-off, and its caller would block on RunComplete until its
// own timeout while the UI showed a stale queued pill on an idle session.
//
// This uses a bounded context so a regression (the queued call's
// RunComplete never arriving) fails the test cleanly instead of hanging
// the suite.
func TestRunTurn_StreamErrorStillDrainsQueue(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &failingStreamModel{}
	notifyBroker := pubsub.NewBroker[notify.Notification]()
	t.Cleanup(notifyBroker.Shutdown)
	runCompleteBroker := pubsub.NewBroker[notify.RunComplete]()
	t.Cleanup(runCompleteBroker.Shutdown)
	sa := NewSessionAgent(SessionAgentOptions{
		Model:       Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 100000, DefaultMaxTokens: 10000}},
		Sessions:    env.sessions,
		Messages:    env.messages,
		Notify:      notifyBroker,
		RunComplete: runCompleteBroker,
	}).(*sessionAgent)
	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	notifications := notifyBroker.Subscribe(ctx)
	completions := runCompleteBroker.Subscribe(ctx)

	// Queue a second, RunID-bearing prompt behind the session before the
	// first turn runs, so it is sitting in the queue when the first
	// turn's Stream call fails.
	sa.enqueueCall(SessionAgentCall{
		SessionID: sess.ID,
		RunID:     "queued",
		Prompt:    "second",
	})

	// Run's loop hands off to the queued call in place of tail recursion
	// (see run's doc comment), so its return value reflects the QUEUED
	// turn, not the failed one - exactly like the summarize-failure case.
	// The failed turn's own error is reported on its own RunComplete,
	// asserted below.
	_, runErr := sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		RunID:     "first",
		Prompt:    "first",
	})
	require.NoError(t, runErr, "the outer turn must not fail just because a queued follow-up came after a failed Stream call")

	// TypeAgentFinished must NOT fire for "first": its Stream call
	// genuinely failed (err != nil), and AgentDispatcher.run (a layer
	// above sessionAgent, not exercised here) is what announces a failed
	// run via TypeAgentError - see completeTurn's err == nil guard.
	// Publishing AgentFinished here too would show "Task finished"
	// immediately alongside "Task failed" for the very same turn.
	//
	// The first notification observed is instead the queue's own
	// change: completeTurn's drainNext hand-off dequeues "queued" to
	// become the next turn, and only that turn's later success (below)
	// earns an AgentFinished of its own.
	select {
	case evt := <-notifications:
		require.Equal(t, notify.TypeQueueChanged, evt.Payload.Type)
		require.Equal(t, sess.ID, evt.Payload.SessionID)
	case <-ctx.Done():
		t.Fatal("timed out waiting for the queue-changed notification from the hand-off")
	}

	// "first"'s own terminal RunComplete must report the Stream failure.
	select {
	case evt := <-completions:
		require.Equal(t, "first", evt.Payload.RunID)
		require.Contains(t, evt.Payload.Error, "stream boom")
	case <-ctx.Done():
		t.Fatal("timed out waiting for the first turn's own RunComplete")
	}

	// The queued RunID-bearing prompt must still be drained and complete
	// on its own, rather than hanging behind the failed turn.
	select {
	case evt := <-completions:
		require.Equal(t, "queued", evt.Payload.RunID)
		require.Empty(t, evt.Payload.Error)
	case <-ctx.Done():
		t.Fatal("timed out waiting for the queued prompt's RunComplete - it hung behind the failed Stream call")
	}

	// And its own turn finishes cleanly, so AgentFinished does fire once
	// for this session - just for the successful hand-off, not the
	// failed turn it followed.
	select {
	case evt := <-notifications:
		require.Equal(t, notify.TypeAgentFinished, evt.Payload.Type)
		require.Equal(t, sess.ID, evt.Payload.SessionID)
	case <-ctx.Done():
		t.Fatal("timed out waiting for AgentFinished after the queued turn's own success")
	}
}
