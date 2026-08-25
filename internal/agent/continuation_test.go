package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// TestStripContinuationPlaceholder_StructuralGuard proves
// stripContinuationPlaceholder's own correctness directly, independent
// of the full turn machinery: it strips the exact shape fantasy's
// createPrompt is documented to produce, and refuses - loudly, via an
// error, never by guessing - anything else, which is the guard against
// trusting position alone that TestDeliverTaskCompletion_
// PlaceholderNeverLeaksToModelOrHistory exercises end to end.
func TestStripContinuationPlaceholder_StructuralGuard(t *testing.T) {
	t.Parallel()

	placeholder := fantasy.NewUserMessage(continuationPromptPlaceholder)
	history := []fantasy.Message{
		fantasy.NewSystemMessage("system"),
		fantasy.NewUserMessage("earlier user turn"),
	}

	t.Run("strips the expected shape", func(t *testing.T) {
		t.Parallel()
		got, err := stripContinuationPlaceholder(append(append([]fantasy.Message{}, history...), placeholder))
		require.NoError(t, err)
		require.Equal(t, history, got)
	})

	t.Run("refuses an empty message list", func(t *testing.T) {
		t.Parallel()
		_, err := stripContinuationPlaceholder(nil)
		require.Error(t, err)
	})

	t.Run("refuses when the last message is not user-role", func(t *testing.T) {
		t.Parallel()
		// e.g. fantasy started appending an assistant-role echo, or
		// reordering the prompt so system comes last.
		messages := append(append([]fantasy.Message{}, history...), fantasy.NewSystemMessage(continuationPromptPlaceholder))
		_, err := stripContinuationPlaceholder(messages)
		require.Error(t, err)
	})

	t.Run("refuses when the last message has more than one part", func(t *testing.T) {
		t.Parallel()
		// e.g. fantasy started attaching provider metadata or an extra
		// part to the synthesized prompt message.
		extra := fantasy.Message{
			Role: fantasy.MessageRoleUser,
			Content: []fantasy.MessagePart{
				fantasy.TextPart{Text: continuationPromptPlaceholder},
				fantasy.TextPart{Text: "something else"},
			},
		}
		messages := append(append([]fantasy.Message{}, history...), extra)
		_, err := stripContinuationPlaceholder(messages)
		require.Error(t, err)
	})

	t.Run("refuses when the text does not match", func(t *testing.T) {
		t.Parallel()
		// e.g. this actually is real content, not the placeholder -
		// exactly the "don't delete real content" failure mode the
		// guard exists to prevent.
		messages := append(append([]fantasy.Message{}, history...), fantasy.NewUserMessage("not the placeholder"))
		_, err := stripContinuationPlaceholder(messages)
		require.Error(t, err)
	})
}

func testCompletion(resultText string) TaskCompletion {
	return TaskCompletion{
		DelegationID:   "task-1",
		Kind:           "task",
		Name:           "task-name",
		Goal:           "background goal",
		Status:         "completed",
		ChildSessionID: "child-session",
		ResultText:     resultText,
	}
}

// TestDeliverTaskCompletion_WakesIdleSessionExactlyOnce proves the core
// wake path: a completion arriving at an idle session starts exactly one
// continuation turn, the model sees the completion in it, and - like the
// mid-turn fold case - nothing about it is persisted as a fabricated
// user message: the placeholder call.Prompt that got the turn started at
// all (continuationPromptPlaceholder) is stripped by PrepareStep before
// the model or the transcript ever see it.
func TestDeliverTaskCompletion_WakesIdleSessionExactlyOnce(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &promptRecordingModel{text: "done"}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	// Give the session prior history ending in an assistant message, the
	// ordinary shape of "a task ran quietly after the assistant's last
	// reply" — proving the wake path supplies its own non-empty
	// placeholder prompt rather than depending on the session already
	// ending in a user/tool message the way an empty fantasy.Call.Prompt
	// would require.
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier answer"}},
	})
	require.NoError(t, err)

	completion := testCompletion("wake-marker-text")
	sa.DeliverTaskCompletion(t.Context(), sess.ID, completion)

	require.Eventually(t, func() bool { return model.count() > 0 }, 2*time.Second, 5*time.Millisecond,
		"the continuation turn must actually reach the model")

	require.Equal(t, 0, sa.QueuedPrompts(sess.ID))
	require.Empty(t, sa.drainCompletionsForStep(sess.ID), "the inbox must be empty: the completion was consumed by the wake, not left behind")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	for _, m := range msgs {
		require.NotEqual(t, message.User, m.Role,
			"the wake path must never persist a fabricated user message, exactly like the mid-turn fold case: got %q", m.Content().String())
	}

	prompts := model.snapshotPrompts()
	require.Len(t, prompts, 1, "exactly one turn must have reached the model")
	var sawCompletion bool
	for _, part := range prompts[0][len(prompts[0])-1].Content {
		if text, ok := part.(fantasy.TextPart); ok {
			if strings.Contains(text.Text, "wake-marker-text") {
				sawCompletion = true
			}
			require.NotContains(t, text.Text, "background delegation continuation",
				"the placeholder prompt must be stripped before the model sees anything")
		}
	}
	require.True(t, sawCompletion, "the model must see the completion")
}

// TestDeliverTaskCompletion_LogsCarryNoPromptOrResultText proves the
// privacy constraint on the delivery path's structured logging: a
// completion's Goal and ResultText must never reach a log line, because
// that content is the user's own work and logs outlive the session. This
// drives a completion through the exact same wake path
// TestDeliverTaskCompletion_WakesIdleSessionExactlyOnce exercises
// (DeliverTaskCompletion -> enqueueCompletion -> startContinuation ->
// prepareStep's drain-and-fold), capturing every slog line emitted along
// the way, and requires both that the model actually saw the completion
// (so a no-op delivery couldn't make this pass vacuously) and that neither
// distinctive marker - the goal text or the result text - appears
// anywhere in the captured logs.
func TestDeliverTaskCompletion_LogsCarryNoPromptOrResultText(t *testing.T) {
	t.Parallel()
	logs := captureLogs(t)

	env := testEnv(t)
	model := &promptRecordingModel{text: "done"}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier answer"}},
	})
	require.NoError(t, err)

	completion := TaskCompletion{
		DelegationID:   "task-1",
		Kind:           "task",
		Name:           "task-name",
		Goal:           "SECRET-GOAL-do-not-log-this-prompt",
		Status:         "completed",
		ChildSessionID: "child-session",
		ResultText:     "SECRET-RESULT-do-not-log-this-either",
	}
	sa.DeliverTaskCompletion(t.Context(), sess.ID, completion)

	require.Eventually(t, func() bool { return model.count() > 0 }, 2*time.Second, 5*time.Millisecond,
		"the continuation turn must actually reach the model")

	// Confirm delivery actually happened - a passing assertion below on an
	// empty log would prove nothing.
	var sawCompletion bool
	for _, prompt := range model.snapshotPrompts() {
		for _, part := range prompt[len(prompt)-1].Content {
			if text, ok := part.(fantasy.TextPart); ok && strings.Contains(text.Text, "SECRET-RESULT-do-not-log-this-either") {
				sawCompletion = true
			}
		}
	}
	require.True(t, sawCompletion, "the model must see the completion for this test to prove anything")

	captured := logs.String()
	require.NotEmpty(t, captured, "the delivery path must actually log something for this test to prove anything")
	require.NotContains(t, captured, "SECRET-GOAL-do-not-log-this-prompt", "a completion's Goal must never reach a log line")
	require.NotContains(t, captured, "SECRET-RESULT-do-not-log-this-either", "a completion's ResultText must never reach a log line")
}

// TestDeliverTaskCompletion_PlaceholderNeverLeaksToModelOrHistory is the
// direct assertion for the assumption stripContinuationPlaceholder
// documents: continuationPromptPlaceholder exists only to satisfy
// fantasy's own createPrompt and must never actually reach the model or
// the transcript. Unlike TestTaskCompletionDelivery_SameContentBothPaths
// (which compares the completion text block between paths and would not
// necessarily notice a leaked placeholder arriving as a *separate*
// message), this scans every message of every prompt the fake model was
// handed, and every persisted message, for the literal placeholder text.
func TestDeliverTaskCompletion_PlaceholderNeverLeaksToModelOrHistory(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &promptRecordingModel{text: "done"}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier answer"}},
	})
	require.NoError(t, err)

	completion := testCompletion("leak-check-marker")
	sa.DeliverTaskCompletion(t.Context(), sess.ID, completion)

	require.Eventually(t, func() bool { return model.count() > 0 }, 2*time.Second, 5*time.Millisecond,
		"the continuation turn must actually reach the model")

	for _, prompt := range model.snapshotPrompts() {
		for _, msg := range prompt {
			for _, part := range msg.Content {
				text, ok := part.(fantasy.TextPart)
				if !ok {
					continue
				}
				require.NotContains(t, text.Text, continuationPromptPlaceholder,
					"the placeholder must never reach the model, in any message of any prompt")
			}
		}
	}

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	for _, m := range msgs {
		require.NotContains(t, m.Content().String(), continuationPromptPlaceholder,
			"the placeholder must never be persisted to history")
	}
}

// TestDeliverTaskCompletion_DuringActiveTurnDoesNotStartSecond proves a
// completion arriving while the session is busy does not start a second
// turn: it stays queued in the inbox for the active turn's own next step
// to drain, exactly as before this step.
func TestDeliverTaskCompletion_DuringActiveTurnDoesNotStartSecond(t *testing.T) {
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
	require.True(t, sa.IsSessionBusy(sess.ID))

	completion := testCompletion("busy-marker")
	sa.DeliverTaskCompletion(t.Context(), sess.ID, completion)

	// Still exactly the one active turn - no second Run was started.
	require.True(t, sa.IsSessionBusy(sess.ID))
	queued := sa.drainCompletionsForStep(sess.ID)
	require.Equal(t, []TaskCompletion{completion}, queued,
		"a completion arriving while busy must stay queued for the active turn's own next step, not vanish or spawn a second turn")

	close(model.gate)
	select {
	case runErr := <-runDone:
		require.NoError(t, runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("run never finished")
	}
}

// concurrencyGuardModel is a plain single-step text-finishing model that
// tracks the maximum number of simultaneous non-title Stream calls it
// ever observed, so a test can assert two turns were never active on the
// same session at once. It also records every non-title call's Prompt,
// like promptRecordingModel: a completion delivered by folding into an
// active turn (runTurn.prepareStep's mid-turn drain) never reaches
// message.Service - only the model actually sees it - so a race test
// checking "was the completion delivered" must inspect what the model
// received, not just what got persisted.
type concurrencyGuardModel struct {
	text string

	active    atomic.Int32
	maxActive atomic.Int32

	mu      sync.Mutex
	prompts []fantasy.Prompt
}

func (m *concurrencyGuardModel) Provider() string { return "fake" }
func (m *concurrencyGuardModel) Model() string    { return "fake-model" }

func (m *concurrencyGuardModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{FinishReason: fantasy.FinishReasonStop}, nil
}

func (m *concurrencyGuardModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	n := m.active.Add(1)
	for {
		old := m.maxActive.Load()
		if n <= old || m.maxActive.CompareAndSwap(old, n) {
			break
		}
	}
	m.mu.Lock()
	m.prompts = append(m.prompts, call.Prompt)
	m.mu.Unlock()
	text := m.text
	return func(yield func(fantasy.StreamPart) bool) {
		defer m.active.Add(-1)
		// Widen the race window: if two turns ever did run concurrently,
		// this gives them time to overlap before either finishes.
		time.Sleep(5 * time.Millisecond)
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

func (m *concurrencyGuardModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *concurrencyGuardModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// occurrences counts how many text parts across every recorded prompt
// contain substr. See promptRecordingModel.occurrences (same idea).
func (m *concurrencyGuardModel) occurrences(substr string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int
	for _, prompt := range m.prompts {
		for _, msg := range prompt {
			for _, part := range msg.Content {
				if text, ok := part.(fantasy.TextPart); ok && strings.Contains(text.Text, substr) {
					n++
				}
			}
		}
	}
	return n
}

// TestDeliverTaskCompletion_RaceWithUserPromptStartsOnlyOneTurn is the
// race the contract calls out explicitly: a completion landing at the
// same instant a user prompt starts a turn must not produce two turns,
// and must not lose the completion. Both triggers are released from the
// same starting gate on a fresh session, repeated across iterations to
// exercise both possible winners of dispatcher.sessionMu.
func TestDeliverTaskCompletion_RaceWithUserPromptStartsOnlyOneTurn(t *testing.T) {
	t.Parallel()
	for i := range 20 {
		env := testEnv(t)
		model := &concurrencyGuardModel{text: "done"}
		sa := NewSessionAgent(SessionAgentOptions{
			Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
			Sessions: env.sessions,
			Messages: env.messages,
		}).(*sessionAgent)

		sess, err := env.sessions.Create(t.Context(), "session")
		require.NoError(t, err)

		userPrompt := fmt.Sprintf("race-user-%d", i)
		completion := testCompletion(fmt.Sprintf("race-marker-%d", i))

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			sa.DeliverTaskCompletion(t.Context(), sess.ID, completion)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _ = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: userPrompt})
		}()
		close(start)
		wg.Wait()

		// The loser of the race (whichever trigger it is) may resolve as
		// a queued follow-up handed off to its own recursive turn, or
		// folded into the winner's own next step, after the winner
		// starts - give that a moment to settle. The user prompt is
		// always persisted (its own createUserMessage call, whether it
		// runs as its own turn or is folded as steering), but a
		// completion folded into an active turn is only ever visible in
		// what the model actually received (runTurn.prepareStep's
		// mid-turn drain never persists it) - see concurrencyGuardModel.
		require.Eventually(t, func() bool {
			return sa.QueuedPrompts(sess.ID) == 0 && !sa.IsSessionBusy(sess.ID) && model.occurrences(completion.ResultText) > 0
		}, 3*time.Second, 5*time.Millisecond, "iteration %d: both the user prompt and the completion must eventually be delivered", i)

		require.LessOrEqual(t, model.maxActive.Load(), int32(1),
			"iteration %d: at most one turn must ever be active on this session at a time", i)

		msgs, err := env.messages.List(t.Context(), sess.ID)
		require.NoError(t, err)
		var userCount int
		for _, m := range msgs {
			if m.Role == message.User && m.Content().String() == userPrompt {
				userCount++
			}
		}
		require.Equal(t, 1, userCount, "iteration %d: the user prompt must be delivered exactly once", i)
		require.Equal(t, 1, model.occurrences(completion.ResultText),
			"iteration %d: the completion must reach the model exactly once, never duplicated", i)
	}
}

// TestDeliverTaskCompletion_CancelledParentDoesNotAutoStart proves that
// once the user has canceled the parent session, a completion arriving
// afterward does not auto-start a continuation: it waits in the inbox
// for the next real user turn, which clears the cancel marker and
// delivers it the ordinary way (drainCompletionsForStep, ahead of
// steering).
func TestDeliverTaskCompletion_CancelledParentDoesNotAutoStart(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &promptRecordingModel{text: "done"}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	// An explicit cancel on an otherwise-idle session: the contract's
	// "the user canceled the parent session" case.
	sa.Cancel(sess.ID)

	completion := testCompletion("should-not-auto-start")
	sa.DeliverTaskCompletion(t.Context(), sess.ID, completion)

	// Give any wrongly-triggered auto-start goroutine a moment to
	// misbehave before asserting its absence.
	time.Sleep(50 * time.Millisecond)
	require.Zero(t, model.count(), "a canceled parent session must not auto-start a continuation")

	// A genuine user turn now: it must clear the cancel marker and
	// deliver the still-queued completion the ordinary way.
	result, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "hello"})
	require.NoError(t, runErr)
	require.NotNil(t, result)

	prompts := model.snapshotPrompts()
	require.Len(t, prompts, 1)
	var sawCompletion, sawUser bool
	for _, msg := range prompts[0] {
		for _, part := range msg.Content {
			if text, ok := part.(fantasy.TextPart); ok {
				if strings.Contains(text.Text, "should-not-auto-start") {
					sawCompletion = true
				}
				if text.Text == "hello" {
					sawUser = true
				}
			}
		}
	}
	require.True(t, sawCompletion, "the completion must be delivered on the next real user turn")
	require.True(t, sawUser, "the user's own prompt must still be present")
}

// probeDepthTool returns a tool that records the cascade depth it
// observed via tools.GetDepthFromContext each time it's called, letting
// a test confirm PrepareStep actually stamps runTurn.call.Depth onto the
// step's context rather than just carrying it on the Go struct.
func probeDepthTool(observed *[]int, mu *sync.Mutex) fantasy.AgentTool {
	return fantasy.NewAgentTool("probe_depth", "reports the current cascade depth", func(ctx context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		mu.Lock()
		*observed = append(*observed, tools.GetDepthFromContext(ctx))
		mu.Unlock()
		return fantasy.NewTextResponse("ok"), nil
	})
}

// toolCallThenFinishModel streams a single tool call to toolName on its
// first non-title step, then a plain text finish on its second.
type toolCallThenFinishModel struct {
	toolName string
	calls    atomic.Int32
}

func (m *toolCallThenFinishModel) Provider() string { return "fake" }
func (m *toolCallThenFinishModel) Model() string    { return "fake-model" }

func (m *toolCallThenFinishModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{FinishReason: fantasy.FinishReasonStop}, nil
}

func (m *toolCallThenFinishModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	n := m.calls.Add(1)
	return func(yield func(fantasy.StreamPart) bool) {
		if n == 1 {
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: "tool", ToolCallName: m.toolName}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: "tool", Delta: `{}`}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: "tool"}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "tool", ToolCallName: m.toolName, ToolCallInput: `{}`})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "done"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *toolCallThenFinishModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *toolCallThenFinishModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// TestPrepareStep_StampsCallDepthOntoContext proves the plumbing half of
// the cascade-depth contract: a turn's SessionAgentCall.Depth reaches a
// tool call through the step's context (tools.DepthContextKey), which is
// what the "agent" tool's background mode reads to gate cascading. This
// is deliberately independent of any real task/completion cascade (slow
// and heavy to build three levels deep with fake streaming models) -
// TestRunBackgroundAgent_CascadeDepthLimit below covers the gate's own
// logic given a depth.
func TestPrepareStep_StampsCallDepthOntoContext(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	var (
		mu       sync.Mutex
		observed []int
	)
	model := &toolCallThenFinishModel{toolName: "probe_depth"}
	sa := testSessionAgent(env, model, "system", probeDepthTool(&observed, &mu)).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "main", Depth: 2})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []int{2}, observed, "the tool call must observe this turn's own Depth via context")
}

// TestRunBackgroundAgent_NestingLimit proves the nesting gate: a turn at
// the hard limit still runs — refusing to delegate further is the tool
// call's own failure, not the turn's — but the delegation tools decline
// with a clear tool error instead of creating a task. Below the limit the
// task is created and stamped with its own depth: one level below the
// turn that started it, which is what makes the *next* level count.
func TestRunBackgroundAgent_NestingLimit(t *testing.T) {
	fake := &fakeTaskManager{info: tools.TaskInfo{ID: "task-1", SessionID: "child-sess", Status: "running"}}
	coord := newAgentToolTestCoordinator(t, fake)

	for depth := range maxTaskCascadeDepth {
		ctx := context.WithValue(t.Context(), tools.DepthContextKey, depth)
		resp, err := coord.runBackgroundAgent(ctx, "parent-sess", fmt.Sprintf("depth %d work", depth), "", depth+1)
		require.NoError(t, err)
		require.False(t, resp.IsError, "depth %d is below the limit and must be allowed", depth)
	}
	require.Len(t, fake.created, maxTaskCascadeDepth)
	for depth, args := range fake.created {
		require.Equal(t, depth+1, args.Depth,
			"a delegation runs one level below the turn that started it")
	}

	// At the limit: the turn itself would still be running (this call
	// simulates the tool invocation inside it), but delegating further is
	// refused.
	limitCtx := context.WithValue(t.Context(), tools.DepthContextKey, maxTaskCascadeDepth)
	resp, err := coord.runBackgroundAgent(limitCtx, "parent-sess", "one too many", "", maxTaskCascadeDepth+1)
	require.NoError(t, err)
	require.True(t, resp.IsError, "a turn at the nesting limit must refuse to delegate further")
	require.Contains(t, resp.Content, "nesting limit")
	require.Len(t, fake.created, maxTaskCascadeDepth, "the refused call must never reach the task manager")

	// A session someone drives — no DepthContextKey set, so
	// GetDepthFromContext defaults to 0 — is at the top level and is
	// allowed again.
	resp, err = coord.runBackgroundAgent(t.Context(), "parent-sess", "fresh user turn", "", 1)
	require.NoError(t, err)
	require.False(t, resp.IsError, "a turn a person drives is at the top level")
	require.Len(t, fake.created, maxTaskCascadeDepth+1)
	require.Equal(t, 1, fake.created[len(fake.created)-1].Depth)
}

// TestRunBackgroundAgent_UnattendedRoundLimit proves the bound that
// actually applies to a loop: rounds, not levels. A session at the top
// level may delegate as many times in a row as it likes — that is an
// iterative plan — until it has run maxUnattendedDelegationRounds of them
// with nobody saying anything, at which point delegating is refused and
// the model is told to report back instead.
func TestRunBackgroundAgent_UnattendedRoundLimit(t *testing.T) {
	fake := &fakeTaskManager{info: tools.TaskInfo{ID: "task-1", SessionID: "child-sess", Status: "running"}}
	coord := newAgentToolTestCoordinator(t, fake)

	// Round after round at depth 0: nesting never grows, so nothing here
	// may be refused for depth. This is the case the old depth-counting
	// gate killed after three rounds.
	for round := range maxUnattendedDelegationRounds {
		ctx := context.WithValue(t.Context(), tools.UnattendedRoundsContextKey, round)
		resp, err := coord.runBackgroundAgent(ctx, "parent-sess", "another round", "", 1)
		require.NoError(t, err)
		require.False(t, resp.IsError, "round %d must be allowed", round)
	}
	require.Len(t, fake.created, maxUnattendedDelegationRounds)

	atCap := context.WithValue(t.Context(), tools.UnattendedRoundsContextKey, maxUnattendedDelegationRounds)
	resp, err := coord.runBackgroundAgent(atCap, "parent-sess", "one round too many", "", 1)
	require.NoError(t, err)
	require.True(t, resp.IsError, "an unattended session must eventually be made to report back")
	require.Contains(t, resp.Content, "without a person in the loop")
	require.Len(t, fake.created, maxUnattendedDelegationRounds)
}

// TestTaskCompletionDelivery_SameContentBothPaths is the regression test
// for the inconsistency the coordinator flagged: the mid-turn fold path
// and the wake path used to record the same event differently (one
// never persisted anything, the other persisted the report text as a
// fabricated user message). Both now go through the exact same
// PrepareStep drain (drainCompletionsForStep + taskCompletionsMessage),
// so the same completion must produce byte-identical model-visible
// content, and neither path may persist a user message for it, whether
// it folds into an already-busy turn or wakes an idle session.
func TestTaskCompletionDelivery_SameContentBothPaths(t *testing.T) {
	t.Parallel()
	completion := testCompletion("parity-marker-text")

	foldedText, foldedUserCount := deliverThroughFold(t, completion)
	wokenText, wokenUserCount := deliverThroughWake(t, completion)

	require.Equal(t, foldedText, wokenText,
		"the same completion must produce identical model-visible content whether it folds into a busy turn or wakes an idle one")
	require.Zero(t, foldedUserCount, "the folded path must not persist a fabricated user message")
	require.Zero(t, wokenUserCount, "the wake path must not persist a fabricated user message")
}

// deliverThroughFold drives completion through the mid-turn fold path -
// an already-active, gated two-step turn, mirroring
// TestPrepareStep_CompletionDeliveredBeforeSteering's setup - and returns
// the exact text of the message the model received for it, plus how many
// persisted user messages mention it (want: 0).
func deliverThroughFold(t *testing.T, completion TaskCompletion) (string, int) {
	t.Helper()
	env := testEnv(t)

	gate := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	hold := fantasy.NewAgentTool("hold", "hold", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		once.Do(func() { close(entered) })
		<-gate
		return fantasy.NewTextResponse("ok"), nil
	})

	model := &twoStepToolModel{}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
		Tools:    []fantasy.AgentTool{hold},
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	runDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "main"})
		runDone <- runErr
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the hold tool never entered - step 1 never reached it")
	}

	sa.DeliverTaskCompletion(t.Context(), sess.ID, completion)
	close(gate)

	select {
	case runErr := <-runDone:
		require.NoError(t, runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("run never finished step 2")
	}

	text := completionMessageText(t, model.lastStep2Prompt(), completion.ResultText)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var userCount int
	for _, m := range msgs {
		if m.Role == message.User && strings.Contains(m.Content().String(), completion.ResultText) {
			userCount++
		}
	}
	return text, userCount
}

// deliverThroughWake drives completion through the wake path - an idle
// session - and returns the exact text of the message the model
// received for it, plus how many persisted user messages mention it
// (want: 0).
func deliverThroughWake(t *testing.T, completion TaskCompletion) (string, int) {
	t.Helper()
	env := testEnv(t)
	model := &promptRecordingModel{text: "done"}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier answer"}},
	})
	require.NoError(t, err)

	sa.DeliverTaskCompletion(t.Context(), sess.ID, completion)
	require.Eventually(t, func() bool { return model.count() > 0 }, 2*time.Second, 5*time.Millisecond)

	prompts := model.snapshotPrompts()
	require.Len(t, prompts, 1)
	text := completionMessageText(t, prompts[0], completion.ResultText)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var userCount int
	for _, m := range msgs {
		if m.Role == message.User && strings.Contains(m.Content().String(), completion.ResultText) {
			userCount++
		}
	}
	return text, userCount
}

// completionMessageText finds the single message part within prompt
// whose text contains marker and returns that text verbatim, failing the
// test if there isn't exactly one such part.
func completionMessageText(t *testing.T, prompt fantasy.Prompt, marker string) string {
	t.Helper()
	var found string
	var n int
	for _, msg := range prompt {
		for _, part := range msg.Content {
			if text, ok := part.(fantasy.TextPart); ok && strings.Contains(text.Text, marker) {
				found = text.Text
				n++
			}
		}
	}
	require.Equal(t, 1, n, "expected exactly one message part containing %q", marker)
	return found
}

// TestUnattendedRounds_CountedPerContinuationAndResetByAPerson proves the
// counter the loop bound rests on: each auto-woken continuation that
// actually becomes the active run is one round, and anything a person
// sends puts it back to zero — which is why a session someone is talking
// to never runs out of rounds.
//
// Driven through dispatchDecision, the one place the counter moves, with
// each decision's active slot released the way a finishing turn releases
// it. Running whole turns instead would prove the same thing through a
// fake model's streaming script, which is a lot of machinery between the
// assertion and what it is about.
func TestUnattendedRounds_CountedPerContinuationAndResetByAPerson(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	sa := testSessionAgent(env, &toolCallThenFinishModel{}, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	run := func(call SessionAgentCall) int {
		call.SessionID = sess.ID
		decision := sa.dispatchDecision(t.Context(), call)
		require.True(t, decision.active, "the session is idle, so every call here becomes the active run")
		sa.clearActiveIfMatch(sess.ID, decision.ac)
		decision.cancel()
		return decision.unattendedRounds
	}

	require.Equal(t, 0, run(SessionAgentCall{Prompt: "do the thing"}),
		"a person's own turn is not a round of unattended work")
	require.Equal(t, 1, run(SessionAgentCall{Prompt: continuationPromptPlaceholder, Continuation: true}))
	require.Equal(t, 2, run(SessionAgentCall{Prompt: continuationPromptPlaceholder, Continuation: true}))
	require.Equal(t, 0, run(SessionAgentCall{Prompt: "carry on"}),
		"a person spoke: the count starts over")
	require.Equal(t, 1, run(SessionAgentCall{Prompt: continuationPromptPlaceholder, Continuation: true}))
}

// TestFoldCompletions_ContinuationStaysAtItsOwnLevel is the regression for
// the loop that died after three rounds. Reacting to a delegation's result
// is the same session at the same level: a completion reports the depth of
// the delegation that produced it, so the session that started it — one
// level up — is where the continuation runs. Deepening here instead made
// an ordinary implement/review/implement plan run out of nesting budget
// after three rounds.
func TestFoldCompletions_ContinuationStaysAtItsOwnLevel(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		completion int
		want       int
	}{
		{"a top-level session's own delegation", 1, 0},
		{"a delegation reacting to its own delegation", 2, 1},
		{"a completion carrying no depth at all", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			sa := testSessionAgent(env, &toolCallThenFinishModel{}, "system").(*sessionAgent)
			sessionID := "session-" + tc.name

			sa.enqueueCompletion(sessionID, TaskCompletion{
				DelegationID: "d1",
				Kind:         "task",
				Status:       "completed",
				Depth:        tc.completion,
			})

			turn := &runTurn{agent: sa, call: SessionAgentCall{SessionID: sessionID, Continuation: true}}
			messages, folded := turn.foldCompletions(nil, 0)
			require.Len(t, folded, 1)
			require.Len(t, messages, 1, "the completion is reported to the model as one message")
			require.Equal(t, tc.want, turn.call.Depth)
		})
	}
}
