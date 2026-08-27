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

// failingSummarizeModel plays two requests: the turn's own step (a plain
// text reply, no tool calls), then the summarize pass, which fails
// outright. Any further request (the queued follow-up draining after
// teardown) succeeds normally.
type failingSummarizeModel struct {
	calls atomic.Int32
}

func (*failingSummarizeModel) Provider() string { return "fake" }
func (*failingSummarizeModel) Model() string    { return "fake-model" }

func (m *failingSummarizeModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *failingSummarizeModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	if m.calls.Add(1) == 2 {
		// The summarize pass: fail outright, deterministically, instead
		// of relying on a busy-slot race.
		return nil, errors.New("summarize boom")
	}
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "done"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *failingSummarizeModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *failingSummarizeModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// TestFinishTurn_SummarizeFailureStillNotifiesAndDrainsQueue is the
// regression test for finishTurn returning early on a summarize error: it
// used to skip both the AgentFinished notification and the drainNext
// handoff, so a prompt already queued behind the session (carrying its own
// RunID) sat forever and its caller blocked on RunComplete indefinitely.
// The fix logs the summarize failure and still runs the same
// release/notify/drain teardown every other turn gets.
//
// This uses a bounded context so a regression (the queued call's
// RunComplete never arriving) fails the test cleanly instead of hanging
// the suite.
func TestFinishTurn_SummarizeFailureStillNotifiesAndDrainsQueue(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &failingSummarizeModel{}
	notifyBroker := pubsub.NewBroker[notify.Notification]()
	t.Cleanup(notifyBroker.Shutdown)
	runCompleteBroker := pubsub.NewBroker[notify.RunComplete]()
	t.Cleanup(runCompleteBroker.Shutdown)
	sa := NewSessionAgent(SessionAgentOptions{
		// A window of 1 forces shouldSummarize on every turn, reaching
		// the summarize call (and its failure) on the very first step.
		Model:       Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 1, DefaultMaxTokens: 10000}},
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
	// first turn runs, so it is sitting in the queue when finishTurn
	// reaches its (about to fail) summarize call.
	sa.enqueueCall(SessionAgentCall{
		SessionID: sess.ID,
		RunID:     "queued",
		Prompt:    "second",
	})

	_, runErr := sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		RunID:     "first",
		Prompt:    "first",
	})
	require.NoError(t, runErr, "the outer turn must not fail just because its own summarize did")

	// The AgentFinished notification must still fire for the turn whose
	// summarize failed.
	select {
	case evt := <-notifications:
		require.Equal(t, notify.TypeAgentFinished, evt.Payload.Type)
		require.Equal(t, sess.ID, evt.Payload.SessionID)
	case <-ctx.Done():
		t.Fatal("timed out waiting for AgentFinished after a failed summarize")
	}

	// "first"'s own terminal RunComplete must report the summarize
	// failure rather than looking like an ordinary success.
	select {
	case evt := <-completions:
		require.Equal(t, "first", evt.Payload.RunID)
		require.Contains(t, evt.Payload.Error, "summarize boom")
	case <-ctx.Done():
		t.Fatal("timed out waiting for the first turn's own RunComplete")
	}

	// The queued RunID-bearing prompt must still be drained and complete
	// on its own, rather than hanging behind the failed summarize.
	select {
	case evt := <-completions:
		require.Equal(t, "queued", evt.Payload.RunID)
		require.Empty(t, evt.Payload.Error)
	case <-ctx.Done():
		t.Fatal("timed out waiting for the queued prompt's RunComplete - it hung behind the failed summarize")
	}
}
