package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/agent/notify"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// TestSummarize_QueuedThenCanceledDoesNotRun is the regression test for the
// bug this refactor closes: Summarize's post-completion handoff used to pop
// the next queued prompt with a raw messageQueue.Get/Set, without ever
// consulting the session's cancel mark - unlike Run's own end-of-turn
// handoff. A prompt queued while Summarize was still streaming and then
// canceled would therefore run anyway. drainNext is now the single
// implementation shared by both handoffs, so Summarize gets the same
// cancel-mark filter Run always had.
//
// model.onFinish fires while Summarize's activeRequests entry is still set
// (i.e. while the session is genuinely busy summarizing), the same timing
// TestRun_AutoSummarizeDoesNotClobberConcurrentActiveRequest uses to land a
// simulated concurrent event inside Summarize's own stream. It enqueues a
// follow-up directly on the dispatcher (mirroring how dispatch_cancel_test.go
// builds state by hand) and raises the cancel mark, without touching
// activeRequests itself - so Summarize's own stream completes normally and
// reaches its post-completion handoff, where the bug lived.
func TestSummarize_QueuedThenCanceledDoesNotRun(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	// Summarize requires at least one existing message to summarize.
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	broker := pubsub.NewBroker[notify.RunComplete]()
	t.Cleanup(broker.Shutdown)
	subCtx, subCancel := context.WithCancel(t.Context())
	defer subCancel()
	ch := broker.Subscribe(subCtx)

	model := &raceInjectModel{text: "summary"}
	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel: Model{
			Model:      model,
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000},
		},
		SystemPrompt: "system",
		Sessions:     env.sessions,
		Messages:     env.messages,
		RunComplete:  broker,
	}).(*sessionAgent)

	// A follow-up is queued while summarization is still active, then a
	// cancel is recorded for the session. Neither touches activeRequests,
	// so Summarize's own stream is unaffected and completes normally.
	model.onFinish = func() {
		require.True(t, sa.IsSessionBusy(sess.ID), "onFinish must fire while Summarize is still active")
		sa.dispatch.enqueueCall(SessionAgentCall{SessionID: sess.ID, RunID: "run-followup", Prompt: "should-not-run"})
		sa.dispatch.cancelMark.Set(sess.ID, 1)
	}

	err = sa.Summarize(t.Context(), sess.ID, fantasy.ProviderOptions{}, nil)
	require.NoError(t, err)

	require.Equal(t, 0, sa.QueuedPrompts(sess.ID), "the canceled follow-up must not remain queued")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	for _, m := range msgs {
		require.NotEqual(t, "should-not-run", m.Content().String(),
			"the canceled follow-up must not have run")
	}

	// The dropped follow-up carried a RunID, so its terminal cancelled
	// RunComplete must still be published - otherwise a caller blocking on
	// that RunID (e.g. `braid run`) would hang.
	select {
	case got := <-ch:
		require.Equal(t, "run-followup", got.Payload.RunID)
		require.Equal(t, sess.ID, got.Payload.SessionID)
		require.True(t, got.Payload.Cancelled)
	case <-time.After(2 * time.Second):
		t.Fatal("expected a cancelled RunComplete for the dropped queued follow-up")
	}
}
