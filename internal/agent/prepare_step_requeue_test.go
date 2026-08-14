package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/rave-soft/braid/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failNthCreate wraps a real message.Service and fails the Nth call to
// Create (1-indexed), regardless of which message it is for. It lets
// tests force createUserMessage to fail partway through PrepareStep's
// fold loop without needing a broken DB connection.
type failNthCreate struct {
	message.Service
	failOn int

	mu    sync.Mutex
	calls int
}

func (f *failNthCreate) Create(ctx context.Context, sessionID string, params message.CreateMessageParams) (message.Message, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if n == f.failOn {
		return message.Message{}, errors.New("simulated create failure")
	}
	return f.Service.Create(ctx, sessionID, params)
}

// TestPrepareStep_FoldPersistFailureRequeuesRemainder covers the
// message-loss defect: when three RunID-less follow-ups are queued for a
// session and persisting the second one fails, the first must already be
// persisted, and the second and third must still be queued afterward, in
// their original order, instead of being dropped on the floor.
func TestPrepareStep_FoldPersistFailureRequeuesRemainder(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	// Call order through PrepareStep is: main user message (1), then the
	// folded follow-ups in FIFO order (2, 3, 4). Failing call 3 lets the
	// first follow-up ("steer-1") persist while "steer-2" fails and
	// "steer-3" is never attempted.
	failing := &failNthCreate{Service: env.messages, failOn: 3}
	env.messages = failing

	model := &finishStreamModel{text: "done"}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "steer-1"})
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "steer-2"})
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "steer-3"})

	result, err := sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "main",
	})
	require.Error(t, err, "the persist failure must propagate out of Run")
	require.Nil(t, result)

	// "steer-1" was persisted before the failure; only it (plus "main")
	// shows up as a user message.
	msgs, listErr := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, listErr)
	var userTexts []string
	for _, m := range msgs {
		if m.Role == message.User {
			userTexts = append(userTexts, m.Content().String())
		}
	}
	assert.Equal(t, []string{"main", "steer-1"}, userTexts,
		"only the main prompt and the successfully persisted follow-up should be recorded")

	// "steer-2" (the one that failed) and "steer-3" (never attempted)
	// must both still be queued, in their original order, not lost.
	assert.Equal(t, []string{"steer-2", "steer-3"}, sa.QueuedPromptsList(sess.ID),
		"unpersisted follow-ups must be requeued in FIFO order, not dropped")
}

// TestPrepareStep_FoldPersistSuccessDrainsQueue is the happy-path
// counterpart: when every folded follow-up persists successfully, the
// queue ends up empty and the follow-ups are recorded in their original
// order.
func TestPrepareStep_FoldPersistSuccessDrainsQueue(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	model := &finishStreamModel{text: "done"}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "steer-1"})
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "steer-2"})
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "steer-3"})

	result, err := sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "main",
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	msgs, listErr := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, listErr)
	var userTexts []string
	for _, m := range msgs {
		if m.Role == message.User {
			userTexts = append(userTexts, m.Content().String())
		}
	}
	assert.Equal(t, []string{"main", "steer-1", "steer-2", "steer-3"}, userTexts,
		"all follow-ups must be persisted in their original order")

	assert.Empty(t, sa.QueuedPromptsList(sess.ID), "queue must be fully drained on the happy path")
}

// TestPrepareStep_FoldPersistFailureRequeuesInAcceptOrder covers a mixed
// queue: RunID-bearing calls (left in the queue by drainQueueForStep,
// since only RunID-less calls fold) interleaved with RunID-less
// follow-ups (which fold). Requeuing a failed fold's remainder must merge
// it back by acceptSeq, not just prepend it, or a RunID-bearing call - a
// `braid run` caller waiting on its own RunComplete - can end up behind a
// steering message typed after it.
func TestPrepareStep_FoldPersistFailureRequeuesInAcceptOrder(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	// Create order: main (1), steer-1 (2, succeeds), steer-2 (3, fails).
	// run-mid-1 and run-mid-2 are never folded (they carry a RunID) so
	// they never reach Create.
	failing := &failNthCreate{Service: env.messages, failOn: 3}
	env.messages = failing

	model := &finishStreamModel{text: "done"}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	// Build the queue directly through BeginAccepted + enqueueCall (the
	// same primitives Run's busy-queue path uses) so each call carries a
	// real acceptSeq in arrival order: steer-1, run-mid-1, steer-2,
	// run-mid-2.
	acceptSteer1 := sa.BeginAccepted(sess.ID)
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "steer-1", Accepted: acceptSteer1})

	acceptRunMid1 := sa.BeginAccepted(sess.ID)
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "run-mid-1", RunID: "run-mid-1-id", Accepted: acceptRunMid1})

	acceptSteer2 := sa.BeginAccepted(sess.ID)
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "steer-2", Accepted: acceptSteer2})

	acceptRunMid2 := sa.BeginAccepted(sess.ID)
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "run-mid-2", RunID: "run-mid-2-id", Accepted: acceptRunMid2})

	result, err := sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "main",
	})
	require.Error(t, err, "the persist failure must propagate out of Run")
	require.Nil(t, result)

	// drainQueueForStep folds steer-1 and steer-2, leaving run-mid-1 and
	// run-mid-2 queued. steer-1 persists; steer-2 fails and must be
	// requeued between run-mid-1 and run-mid-2, its accept-order
	// position, not shoved back in front of both.
	assert.Equal(t, []string{"run-mid-1", "steer-2", "run-mid-2"}, sa.QueuedPromptsList(sess.ID),
		"the requeued remainder must merge back by acceptSeq, not just prepend")
}
