package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// TestRun_CreateUserMessageFailurePublishesTerminalEvent is the
// regression test for a silent-discard gap in runTurn: a call became the
// active run (dispatchDecision's idle branch fired, so the caller has
// every reason to believe the prompt is in flight), but then
// createUserMessage failed - and the function returned before the
// completionReporter that publishes RunComplete even existed. The prompt
// vanished with no trace: nothing persisted, and no terminal event for a
// caller waiting on this RunID (e.g. `sennit run`, which blocks on
// RunComplete rather than polling messages). session.Get and
// getSessionMessages failing earlier in the same function are the same
// gap; createUserMessage is the simplest of the three to provoke
// deterministically with failNthCreate (defined in
// prepare_step_requeue_test.go), and all three shared one fix: construct
// the reporter, and register the defer that publishes through it, before
// any of this fallible setup runs rather than after.
func TestRun_CreateUserMessageFailurePublishesTerminalEvent(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	failing := &failNthCreate{Service: env.messages, failOn: 1}
	env.messages = failing

	broker := pubsub.NewBroker[notify.RunComplete]()
	t.Cleanup(broker.Shutdown)

	sa := NewSessionAgent(SessionAgentOptions{
		Model:       Model{Model: &finishStreamModel{text: "done"}, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		Sessions:    env.sessions,
		Messages:    env.messages,
		RunComplete: broker,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	subCtx, subCancel := context.WithCancel(t.Context())
	defer subCancel()
	ch := broker.Subscribe(subCtx)

	_, runErr := sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		RunID:     "run-doomed",
		Prompt:    "the user's actual prompt",
	})
	require.Error(t, runErr, "the createUserMessage failure must still be reported to the immediate caller")

	select {
	case ev := <-ch:
		require.Equal(t, "run-doomed", ev.Payload.RunID, "the terminal event must be attributed to the failed prompt's own RunID")
		require.NotEmpty(t, ev.Payload.Error, "the terminal event must carry the failure that discarded the prompt")
	case <-time.After(2 * time.Second):
		t.Fatal("no terminal RunComplete was published for the failed prompt - it vanished with no trace, exactly the silent-discard gap this test guards against")
	}

	require.False(t, sa.IsSessionBusy(sess.ID), "the session must not be left marked busy by a turn that failed this early")

	msgs, listErr := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, listErr)
	require.Empty(t, msgs, "nothing should be persisted for a prompt whose own message creation failed")
}
