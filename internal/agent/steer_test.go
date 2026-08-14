package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/agent/notify"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// toolStepModel drives a two-step turn: step 1 calls the "hold" tool
// (whose handler is supplied by the test and can block); step 2 - reached
// only after the tool call resolves and PrepareStep runs again - streams
// plain text and finishes. It exists so a test can hold a turn "busy"
// (inside the blocked tool call) long enough to enqueue a follow-up, then
// release it and observe that follow-up get folded into step 2 via
// PrepareStep's queue drain, rather than starting a second stream.
type toolStepModel struct {
	text   string
	steps  atomic.Int32
	toolID string
}

func (m *toolStepModel) Provider() string { return "fake" }
func (m *toolStepModel) Model() string    { return "fake-model" }

func (m *toolStepModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{FinishReason: fantasy.FinishReasonStop}, nil
}

func (m *toolStepModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	step := m.steps.Add(1)
	return func(yield func(fantasy.StreamPart) bool) {
		if step == 1 {
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: m.toolID, ToolCallName: "hold"}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: m.toolID, Delta: `{}`}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: m.toolID}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: m.toolID, ToolCallName: "hold", ToolCallInput: `{}`})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			return
		}
		text := m.text
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: text}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *toolStepModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *toolStepModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// TestSteer_BusyFoldsIntoActiveTurn proves the busy path: while a turn is
// active (mid tool call), Steer must enqueue the follow-up rather than
// start a second stream, and that follow-up must be folded into the
// active turn's next step (PrepareStep's queue drain) instead of running
// as its own turn.
func TestSteer_BusyFoldsIntoActiveTurn(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	toolEntered := make(chan struct{})
	toolGate := make(chan struct{})
	hold := fantasy.NewAgentTool("hold", "hold", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		close(toolEntered)
		<-toolGate
		return fantasy.NewTextResponse("ok"), nil
	})

	model := &toolStepModel{text: "done", toolID: "tool-1"}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
		Tools:    []fantasy.AgentTool{hold},
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	mainDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "main"})
		mainDone <- runErr
	}()

	// Wait until step 1's tool call is in flight: the session is busy but
	// no follow-up has been folded yet (PrepareStep for step 2 hasn't run).
	select {
	case <-toolEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("main run never entered the tool call")
	}
	require.True(t, sa.IsSessionBusy(sess.ID), "the main turn must be active before Steer is called")

	outcome, res, err := sa.Steer(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "steer"})
	require.NoError(t, err)
	require.Equal(t, SteerEnqueued, outcome, "Steer must report that a busy-session call was enqueued, not run")
	require.Nil(t, res, "a busy-session Steer call must enqueue and return (nil, nil)")
	require.Equal(t, 1, sa.QueuedPrompts(sess.ID), "Steer must queue behind the active turn, not start a second stream")

	// Release the tool call so step 2 runs; PrepareStep drains the queue
	// and folds "steer" into this same turn.
	close(toolGate)
	require.NoError(t, <-mainDone)

	require.Equal(t, int32(2), model.steps.Load(), "exactly one turn (two steps), never a second stream")
	require.Equal(t, 0, sa.QueuedPrompts(sess.ID), "the folded follow-up must be drained from the queue")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var userTexts []string
	var assistants int
	for _, m := range msgs {
		switch m.Role {
		case message.User:
			userTexts = append(userTexts, m.Content().String())
		case message.Assistant:
			assistants++
		}
	}
	require.Equal(t, []string{"main", "steer"}, userTexts,
		"the follow-up must be persisted as a second user message in the same turn")
	// toolStepModel's two steps each produce their own assistant message
	// row (PrepareStep creates one per step); a folded follow-up adds no
	// step of its own. A follow-up that instead ran as its own recursive
	// turn would add a third.
	require.Equal(t, int(model.steps.Load()), assistants,
		"the follow-up must be folded into the active turn's steps, not run as a separate turn")
}

// TestSteer_IdleRunsAsNewTurn proves the idle path: with no active turn,
// Steer must run the call itself instead of queueing it.
func TestSteer_IdleRunsAsNewTurn(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	model := &finishStreamModel{text: "done"}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	require.False(t, sa.IsSessionBusy(sess.ID))
	outcome, res, err := sa.Steer(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "steer"})
	require.NoError(t, err)
	require.Equal(t, SteerRan, outcome, "Steer must report that an idle-session call ran as its own turn")
	require.NotNil(t, res, "an idle-session Steer call must run its own turn and return a result")

	require.Equal(t, 0, sa.QueuedPrompts(sess.ID))
	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, message.User, msgs[0].Role)
	require.Equal(t, "steer", msgs[0].Content().String())
	require.Equal(t, message.Assistant, msgs[1].Role)
}

// TestSteer_LandingAsTurnFinishesRunsExactlyOnce targets the race
// Steer's atomicity exists for: a follow-up that lands exactly as the
// active turn finishes must run exactly once, whether it gets folded
// into a recursive hand-off or ends up starting its own turn because the
// session looked idle for an instant.
//
// Forcing the interleaving: Run's end-of-turn sequence (agent.go) does,
// in order: (1) CompareAndDelete the activeRequests entry - after this
// point IsSessionBusy reports false; (2) publish the TypeAgentFinished
// notification; (3) drainNext, which hands off to anything queued. A
// custom notify.Publisher blocks inside step (2), so pausing there
// deterministically parks the main turn's goroutine strictly after (1)
// and strictly before (3) - the exact idle gap a concurrent Steer would
// need to land in to race the hand-off, not a window this test has to
// hope a goroutine schedules into. With the main goroutine parked there,
// this test calls the real Steer method from a second, genuinely live
// goroutine; since activeRequests is already clear, Steer's own busy
// check (under the same per-session dispatch mutex) observes idle and
// runs the follow-up itself, before the parked turn is released to reach
// drainNext and find nothing left to hand off to.
type blockingNotifier struct {
	published chan struct{}
	release   chan struct{}
	blocked   atomic.Bool
}

// Publish blocks only on the first call (the parked main turn's own
// TypeAgentFinished notification). The follow-up turn Steer starts while
// the main turn is parked here publishes its own TypeAgentFinished
// notification too and must not block on it - sync.Once would still
// serialize that second call behind the first's in-flight (blocked)
// invocation, deadlocking the two turns against each other, so this
// uses a CompareAndSwap instead: only the call that wins it blocks:
// every other caller, including one arriving while the winner is still
// parked, returns immediately.
func (p *blockingNotifier) Publish(pubsub.EventType, notify.Notification) {
	if p.blocked.CompareAndSwap(false, true) {
		close(p.published)
		<-p.release
	}
}

func (*blockingNotifier) PublishMustDeliver(context.Context, pubsub.EventType, notify.Notification) {}

func TestSteer_LandingAsTurnFinishesRunsExactlyOnce(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	model := &finishStreamModel{text: "main-done"}
	notifier := &blockingNotifier{published: make(chan struct{}), release: make(chan struct{})}
	broker := pubsub.NewBroker[notify.RunComplete]()
	t.Cleanup(broker.Shutdown)
	sa := NewSessionAgent(SessionAgentOptions{
		Model:       Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		Sessions:    env.sessions,
		Messages:    env.messages,
		Notify:      notifier,
		RunComplete: broker,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	subCtx, subCancel := context.WithCancel(t.Context())
	defer subCancel()
	completions := broker.Subscribe(subCtx)

	mainDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, RunID: "main", Prompt: "main"})
		mainDone <- runErr
	}()

	// Wait until the main turn is parked strictly between clearing
	// activeRequests and draining the queue.
	select {
	case <-notifier.published:
	case <-time.After(5 * time.Second):
		t.Fatal("main run never reached the post-cleanup notification")
	}
	require.False(t, sa.IsSessionBusy(sess.ID),
		"activeRequests must already be clear at this point for the race to be meaningful")

	type steerCallResult struct {
		outcome SteerOutcome
		res     *fantasy.AgentResult
		err     error
	}
	steerDone := make(chan steerCallResult, 1)
	go func() {
		outcome, res, steerErr := sa.Steer(t.Context(), SessionAgentCall{SessionID: sess.ID, RunID: "steer", Prompt: "steer"})
		steerDone <- steerCallResult{outcome, res, steerErr}
	}()

	var steerResult steerCallResult
	select {
	case steerResult = <-steerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Steer never completed while the main turn was parked")
	}
	require.NoError(t, steerResult.err)
	// The whole point of parking the main turn strictly after
	// activeRequests is cleared: a racing Steer must observe idle, not
	// busy, and its reported outcome must match what it actually did
	// (ran its own turn), not just what it returned.
	require.Equal(t, SteerRan, steerResult.outcome,
		"Steer must report that it observed the session as idle and ran its own turn")
	require.NotNil(t, steerResult.res, "Steer must have observed the session as idle and run its own turn")
	require.Equal(t, 0, sa.QueuedPrompts(sess.ID),
		"Steer must not have enqueued: it ran directly instead")

	// Release the parked main turn; its own drainNext must find nothing
	// left to hand off to.
	close(notifier.release)
	require.NoError(t, <-mainDone)

	got := map[string]notify.RunComplete{}
	deadline := time.After(5 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-completions:
			if _, dup := got[ev.Payload.RunID]; dup {
				t.Fatalf("duplicate RunComplete for RunID %q", ev.Payload.RunID)
			}
			got[ev.Payload.RunID] = ev.Payload
		case <-deadline:
			t.Fatalf("timed out waiting for both RunCompletes; got %v", got)
		}
	}
	require.False(t, got["main"].Cancelled)
	require.False(t, got["steer"].Cancelled)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var userTexts []string
	var assistants int
	for _, m := range msgs {
		switch m.Role {
		case message.User:
			userTexts = append(userTexts, m.Content().String())
		case message.Assistant:
			assistants++
		}
	}
	require.ElementsMatch(t, []string{"main", "steer"}, userTexts,
		"both prompts must be persisted exactly once - never dropped, never run twice")
	require.Equal(t, 2, assistants,
		"two independent turns must each produce exactly one assistant message - never a missing or duplicate turn")
}
