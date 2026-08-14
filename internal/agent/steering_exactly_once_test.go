package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/message"
	"github.com/stretchr/testify/require"
)

// countSteerMessages returns how many User-role messages in msgs carry
// the "steer" text these tests fold in, so the exactly-once assertions
// below hold regardless of how many other messages (assistant steps,
// tool results, summaries) surround it.
func countSteerMessages(msgs []message.Message) int {
	var n int
	for _, m := range msgs {
		if m.Role == message.User && m.Content().String() == "steer" {
			n++
		}
	}
	return n
}

// TestSteeringExactlyOnce_AutoSummarizeContinuation covers case 1: a
// steering follow-up folded into a step that then triggers auto-
// summarize and a continuation turn (the step's assistant message had
// unfinished tool calls). The steering text must appear in persisted
// history exactly once - createUserMessage runs once, during the
// original step's PrepareStep, before summarize or the continuation
// exist - and it must not be replayed a second time into the
// continuation's model context, because getSessionMessages truncates
// history to start at the summary once session.SummaryMessageID is set
// (see getSessionMessages, agent.go).
//
// continuationRaceModel (summarize_race_test.go) drives the natural
// cascade this scenario produces: stream 1 is the main turn's tool-call
// step (triggers shouldSummarize via ContextWindow: 1 and leaves a
// pending tool call, so run's tail requeues a continuation); stream 2 is
// summarize's own pass over that turn (gated so the test can assert
// mid-flight); stream 3 is the continuation turn's step (plain text, no
// tool calls); stream 4 is that continuation's own summarize pass (cw:1
// forces it again, but it requeues nothing further since stream 3 left
// no pending tool calls) - so the chain terminates on its own without
// the test needing to drive it past the one gate.
func TestSteeringExactlyOnce_AutoSummarizeContinuation(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	model := &continuationRaceModel{summaryStarted: make(chan struct{}), releaseSummary: make(chan struct{})}
	hold := fantasy.NewAgentTool("hold", "hold", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 1, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
		Tools:    []fantasy.AgentTool{hold},
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	// Queue the steering follow-up before the turn starts so PrepareStep
	// folds and persists it as part of step 1, alongside "main".
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "steer"})

	runDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{
			SessionID: sess.ID,
			RunID:     "main",
			Prompt:    "main",
		})
		runDone <- runErr
	}()

	select {
	case <-model.summaryStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("summarize never started")
	}
	require.Equal(t, 0, sa.QueuedPrompts(sess.ID),
		"the steering follow-up must already be drained by step 1's PrepareStep, before summarize even runs")

	close(model.releaseSummary)

	select {
	case err := <-runDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("run never completed the summarize + continuation chain")
	}

	require.Equal(t, 0, sa.QueuedPrompts(sess.ID), "nothing must be left queued once the chain finishes")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, 1, countSteerMessages(msgs),
		"the steering message must appear in persisted history exactly once across summarize and the continuation turn")
}

// cancelAwareGatedModel blocks the first (non-title) Stream call until
// released, and - unlike gatedStreamModel - returns ctx.Err() directly
// if ctx is canceled while blocked, instead of proceeding to yield a
// normal success stream regardless of cancellation. That makes it
// deterministic for testing genuine mid-stream cancellation: the test
// does not have to rely on the fantasy SDK noticing cancellation on its
// own from a fake model that never does real I/O.
type cancelAwareGatedModel struct {
	text    string
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (m *cancelAwareGatedModel) Provider() string { return "fake" }
func (m *cancelAwareGatedModel) Model() string    { return "fake-model" }

func (m *cancelAwareGatedModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{FinishReason: fantasy.FinishReasonStop}, nil
}

func (m *cancelAwareGatedModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	m.once.Do(func() { close(m.entered) })
	select {
	case <-m.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	text := m.text
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: text}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *cancelAwareGatedModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *cancelAwareGatedModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// TestSteeringExactlyOnce_CancelDuringStep covers case 2: a steering
// follow-up folded into a step that is then canceled mid-stream. By the
// time Cancel runs, the follow-up was already drained from the queue by
// PrepareStep (drainQueueForStep only ever removes, under the dispatch
// mutex, at the start of a step) and persisted as its own user message,
// so Cancel - which only clears what is still queued and cancels the
// active context - has nothing left to touch for it: no requeue path
// exists between "drained and persisted" and "step canceled". The
// steering message must survive in history exactly once, and the queue
// must stay empty so it cannot run a second time.
func TestSteeringExactlyOnce_CancelDuringStep(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	model := &cancelAwareGatedModel{
		text:    "done",
		gate:    make(chan struct{}),
		entered: make(chan struct{}),
	}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "steer"})

	runDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "main"})
		runDone <- runErr
	}()

	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("main run never entered Stream")
	}

	// PrepareStep already ran (it runs before Stream is called), so the
	// follow-up is already drained and persisted before the cancel below.
	require.Equal(t, 0, sa.QueuedPrompts(sess.ID))
	preCancelMsgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, 1, countSteerMessages(preCancelMsgs),
		"the steering message must already be persisted before the cancel arrives")

	sa.Cancel(sess.ID)

	select {
	case runErr := <-runDone:
		require.Error(t, runErr, "a canceled step must surface as an error from Run")
	case <-time.After(5 * time.Second):
		t.Fatal("run never returned after cancel")
	}

	require.Equal(t, 0, sa.QueuedPrompts(sess.ID), "the steering follow-up must not reappear in the queue after cancel")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, 1, countSteerMessages(msgs),
		"the steering message must still appear in history exactly once after the cancel")

	var assistants int
	for _, m := range msgs {
		if m.Role == message.Assistant {
			assistants++
			require.Equal(t, message.FinishReasonCanceled, m.FinishReason())
		}
	}
	require.Equal(t, 1, assistants, "the canceled step must produce exactly one assistant message")
}

// failFirstStreamModel returns a plain error from the first (non-title)
// Stream call, simulating a step that fails outright after PrepareStep
// already succeeded - as opposed to a cancellation.
type failFirstStreamModel struct {
	err error
}

func (m *failFirstStreamModel) Provider() string { return "fake" }
func (m *failFirstStreamModel) Model() string    { return "fake-model" }

func (m *failFirstStreamModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, m.err
}

func (m *failFirstStreamModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	return nil, m.err
}

func (m *failFirstStreamModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *failFirstStreamModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// TestSteeringExactlyOnce_StepErrorThenRetry covers case 3: PrepareStep
// succeeds (the steering follow-up is drained and persisted) but the
// model call for that same step fails outright - a different failure
// mode than the requeue-on-persist-failure case already covered by
// TestPrepareStep_FoldPersistFailureRequeuesRemainder, since here
// nothing about persisting the fold failed; the model call after it did.
// drainQueueForStep already removed the follow-up from the queue
// (dispatch.go's fold vs keep partition happens once, at the start of
// the step, regardless of how that step later turns out), so there is
// nothing left to fold again - not on this failed attempt, and not on a
// subsequent independent retry Run for the same session.
func TestSteeringExactlyOnce_StepErrorThenRetry(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	model := &failFirstStreamModel{err: errors.New("boom")}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "steer"})

	_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "main"})
	require.Error(t, runErr)

	require.Equal(t, 0, sa.QueuedPrompts(sess.ID),
		"the steering follow-up must not be left in (or put back into) the queue by a step failure that happens after PrepareStep succeeded")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, 1, countSteerMessages(msgs),
		"the steering message must be persisted exactly once despite the step failing")
	var errored int
	for _, m := range msgs {
		if m.Role == message.Assistant {
			errored++
			require.Equal(t, message.FinishReasonError, m.FinishReason())
		}
	}
	require.Equal(t, 1, errored)

	// A user retry is a brand new top-level call, independent of the
	// failed one; it must not re-fold "steer" from the queue (it is not
	// there) and must not duplicate it in history.
	_, retryErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "main-retry"})
	require.Error(t, retryErr, "the retry uses the same failing model and fails too, which is fine - only the fold behavior is under test")

	require.Equal(t, 0, sa.QueuedPrompts(sess.ID))
	msgsAfterRetry, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, 1, countSteerMessages(msgsAfterRetry),
		"the steering message must still be exactly one after an unrelated retry on the same session")
}
