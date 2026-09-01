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

	// The wake path only ever starts the session this sennit is
	// working in (see dispatcher.wakeAllowed); this is that session.
	sa.SetLiveSession(sess.ID)

	completion := testCompletion("wake-marker-text")
	sa.DeliverTaskCompletion(t.Context(), sess.ID, completion)

	require.Eventually(t, func() bool { return model.count() > 0 }, 2*time.Second, 5*time.Millisecond,
		"the continuation turn must actually reach the model")

	require.Equal(t, 0, sa.QueuedPrompts(sess.ID))
	require.Empty(t, sa.drainCompletionsForStep(sess.ID), "the inbox must be empty: the completion was consumed by the wake, not left behind")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var reports int
	for _, m := range msgs {
		if m.Role != message.User {
			continue
		}
		require.Equal(t, message.OriginAgent, m.Origin,
			"the wake path must never persist a report as something the person typed: got %q", m.Content().String())
		reports++
	}
	require.Equal(t, 1, reports,
		"the report is persisted exactly once, as agent-authored history, exactly like the mid-turn fold case")

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

	sa.SetLiveSession(sess.ID)

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

	sa.SetLiveSession(sess.ID)

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
			return sa.QueuedPrompts(sess.ID) == 0 && !sa.IsSessionBusy(sess.ID)
		}, 3*time.Second, 5*time.Millisecond, "iteration %d: the race must settle with nothing queued and no turn left running", i)

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

		// The completion must survive the race exactly once - but which
		// of its two resting places it lands in is genuinely the race's
		// to decide, and both are correct. It reaches the model if it
		// made it into the inbox before the user's turn drained it in
		// prepareStep, or if the wake attempt won and ran a
		// continuation of its own; landing between those leaves it
		// queued for the next turn instead, since a wake attempt on a
		// session that is already busy drops rather than queues (see
		// startContinuation). Asserting delivery outright would be
		// asserting one particular winner of the very race this test
		// exists to allow either way.
		delivered := model.occurrences(completion.ResultText)
		queued := completionInboxLen(sa, sess.ID)
		require.Equal(t, 1, delivered+queued,
			"iteration %d: the completion must rest in exactly one place - delivered to the model (%d) or left queued in the inbox (%d) - never lost, never duplicated",
			i, delivered, queued)
	}
}

// completionInboxLen reports how many completions are still waiting in
// sessionID's inbox, for the tests that have to tell "left queued for
// the next real turn" apart from "lost".
func completionInboxLen(sa *sessionAgent, sessionID string) int {
	s, release := sa.session(sessionID)
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.completionInbox)
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

// TestDeliverTaskCompletion_PersonsSessionIsWokenToo is the rule the wake
// path exists for: a delegation finishing has to reach the session that
// started it, and the session a person drives is the one that started
// most of them. It wakes on the report the same way a delegation's own
// session does — which is what lets an orchestrating turn hand the work
// to the next delegation without the person typing between them.
//
// The cost is deliberate and stated in docs/concepts/delegation.md: a
// session left alone keeps working on what it was already given. Esc
// twice stops it; options.background_agents turns delegation off.
func TestDeliverTaskCompletion_PersonsSessionIsWokenToo(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &promptRecordingModel{text: "done"}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	// Prior history ending in an assistant message: the ordinary shape of
	// "a delegation finished after the assistant's last reply". No
	// RegisterDelegationParent call - this is a person's own session.
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier answer"}},
	})
	require.NoError(t, err)

	sa.SetLiveSession(sess.ID)

	completion := testCompletion("reaches-the-person's-session")
	sa.DeliverTaskCompletion(t.Context(), sess.ID, completion)

	require.Eventually(t, func() bool { return model.count() > 0 }, 2*time.Second, 5*time.Millisecond,
		"a completion must wake the session that started the delegation")

	prompts := model.snapshotPrompts()
	require.Len(t, prompts, 1, "exactly one continuation turn must have reached the model")
	require.True(t, promptContains(prompts[0], "reaches-the-person's-session"),
		"the woken turn must carry the completion")
	require.Empty(t, sa.drainCompletionsForStep(sess.ID), "that turn must have drained the inbox")

	// The placeholder that got the turn started is never persisted as a
	// prompt the person did not type.
	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	for _, m := range msgs {
		require.NotContains(t, m.Content().String(), continuationPromptPlaceholder)
	}
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
		resp, err := coord.delegation.runBackgroundAgent(ctx, "parent-sess", fmt.Sprintf("depth %d work", depth), "", depth+1)
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
	resp, err := coord.delegation.runBackgroundAgent(limitCtx, "parent-sess", "one too many", "", maxTaskCascadeDepth+1)
	require.NoError(t, err)
	require.True(t, resp.IsError, "a turn at the nesting limit must refuse to delegate further")
	require.Contains(t, resp.Content, "nesting limit")
	require.Len(t, fake.created, maxTaskCascadeDepth, "the refused call must never reach the task manager")

	// A session someone drives — no DepthContextKey set, so
	// GetDepthFromContext defaults to 0 — is at the top level and is
	// allowed again.
	resp, err = coord.delegation.runBackgroundAgent(t.Context(), "parent-sess", "fresh user turn", "", 1)
	require.NoError(t, err)
	require.False(t, resp.IsError, "a turn a person drives is at the top level")
	require.Len(t, fake.created, maxTaskCascadeDepth+1)
	require.Equal(t, 1, fake.created[len(fake.created)-1].Depth)
}

// TestRunBackgroundAgent_RepeatedRoundsAtTopLevelAllowed pins the other
// half of "depth counts nesting": a session at the top level may delegate
// as many times in a row as the work needs. That is what an iterative
// plan looks like — implement, review, implement again — and it is what
// the old depth-counting gate killed after three rounds. Nothing bounds
// how many such rounds a session runs.
func TestRunBackgroundAgent_RepeatedRoundsAtTopLevelAllowed(t *testing.T) {
	fake := &fakeTaskManager{info: tools.TaskInfo{ID: "task-1", SessionID: "child-sess", Status: "running"}}
	coord := newAgentToolTestCoordinator(t, fake)

	const rounds = maxTaskCascadeDepth * 10
	for round := range rounds {
		resp, err := coord.delegation.runBackgroundAgent(t.Context(), "parent-sess", "another round", "", 1)
		require.NoError(t, err)
		require.False(t, resp.IsError, "round %d must be allowed", round)
	}
	require.Len(t, fake.created, rounds)
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

	foldedText, foldedPersonCount, foldedReports := deliverThroughFold(t, completion)
	wokenText, wokenPersonCount, wokenReports := deliverThroughWake(t, completion)

	require.Equal(t, foldedText, wokenText,
		"the same completion must produce identical model-visible content whether it folds into a busy turn or wakes an idle one")
	require.Zero(t, foldedPersonCount, "the folded path must not persist the report as something the person typed")
	require.Zero(t, wokenPersonCount, "the wake path must not persist the report as something the person typed")
	require.Equal(t, 1, foldedReports, "the folded path persists the report exactly once, as agent-authored history")
	require.Equal(t, 1, wokenReports, "the wake path persists the report exactly once, as agent-authored history")
}

// deliverThroughFold drives completion through the mid-turn fold path -
// an already-active, gated two-step turn, mirroring
// TestPrepareStep_CompletionDeliveredBeforeSteering's setup - and returns
// the exact text of the message the model received for it, plus how many
// persisted messages mention it, split into person-authored (want: 0)
// and agent-authored reports (want: 1).
func deliverThroughFold(t *testing.T, completion TaskCompletion) (string, int, int) {
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
	personCount, reports := countCompletionMessages(msgs, completion.ResultText)
	return text, personCount, reports
}

// deliverThroughWake drives completion through the wake path - an idle
// session - and returns the exact text of the message the model
// received for it, plus how many persisted messages mention it, split
// into person-authored (want: 0) and agent-authored reports (want: 1).
func deliverThroughWake(t *testing.T, completion TaskCompletion) (string, int, int) {
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

	sa.SetLiveSession(sess.ID)

	sa.DeliverTaskCompletion(t.Context(), sess.ID, completion)
	require.Eventually(t, func() bool { return model.count() > 0 }, 2*time.Second, 5*time.Millisecond)

	prompts := model.snapshotPrompts()
	require.Len(t, prompts, 1)
	text := completionMessageText(t, prompts[0], completion.ResultText)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	personCount, reports := countCompletionMessages(msgs, completion.ResultText)
	return text, personCount, reports
}

// countCompletionMessages splits the persisted user-role messages
// mentioning marker into those attributed to the person (which a
// delegation report must never be) and the agent-authored reports
// themselves.
func countCompletionMessages(msgs []message.Message, marker string) (person, reports int) {
	for _, m := range msgs {
		if m.Role != message.User || !strings.Contains(m.Content().String(), marker) {
			continue
		}
		if m.Origin == message.OriginAgent {
			reports++
			continue
		}
		person++
	}
	return person, reports
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
			// A real session row: the fold persists the report as
			// session history now, so a made-up id fails the
			// messages table's foreign key instead of exercising
			// the depth rule under test.
			sess, err := env.sessions.Create(t.Context(), tc.name)
			require.NoError(t, err)
			sessionID := sess.ID

			sa.enqueueCompletion(sessionID, TaskCompletion{
				DelegationID: "d1",
				Kind:         "task",
				Status:       "completed",
				Depth:        tc.completion,
			})

			turn := &runTurn{agent: sa, call: SessionAgentCall{SessionID: sessionID, Continuation: true}}
			messages, folded, err := turn.foldCompletions(t.Context(), nil, 0)
			require.NoError(t, err)
			require.Len(t, folded, 1)
			require.Len(t, messages, 1, "the completion is reported to the model as one message")
			require.Equal(t, tc.want, turn.call.Depth)
		})
	}
}

// TestDeliverTaskCompletion_OtherSessionIsNeverWoken is the rule the wake
// gate exists for, written from the day it was missing: four sessions
// nobody was working in woke at once when a restart recovered their
// interrupted delegations, and each re-ran a pipeline whose work was
// long since committed.
//
// A sennit works in one session; every other conversation in the
// database stays where it was left. Its completion is not lost - it is
// folded into the top of that session's next turn, the first moment it
// is being worked in again.
func TestDeliverTaskCompletion_OtherSessionIsNeverWoken(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &promptRecordingModel{text: "done"}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	left, err := env.sessions.Create(t.Context(), "left days ago")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), left.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier answer"}},
	})
	require.NoError(t, err)

	// This sennit is working in an entirely different session.
	sa.SetLiveSession("a-different-session")

	sa.DeliverTaskCompletion(t.Context(), left.ID, testCompletion("must-not-restart-this"))

	time.Sleep(50 * time.Millisecond)
	require.Zero(t, model.count(), "a session nobody is working in must never start a turn")
	require.False(t, sa.IsSessionBusy(left.ID))

	// Their own next turn in it is what delivers the report.
	_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: left.ID, Prompt: "hello"})
	require.NoError(t, runErr)

	prompts := model.snapshotPrompts()
	require.Len(t, prompts, 1, "exactly one turn must have reached the model")
	require.True(t, promptContains(prompts[0], "must-not-restart-this"),
		"the queued completion must reach the model on the person's next turn")
	require.Empty(t, sa.drainCompletionsForStep(left.ID), "that turn must have drained the inbox")
}

// TestWakeAllowed_FollowsTheDelegationChainToTheLiveSession pins what
// the live session's tree covers: the session itself, and a delegation
// of a delegation of it, however many levels down - because nobody sits
// in those to type, and parking them forever would hold a concurrency
// slot and never answer the parent. Anything rooted elsewhere is
// refused.
func TestWakeAllowed_FollowsTheDelegationChainToTheLiveSession(t *testing.T) {
	t.Parallel()
	d := newDispatcher()

	d.RegisterDelegationParent("child", DelegationParent{ParentSessionID: "live"})
	d.RegisterDelegationParent("grandchild", DelegationParent{ParentSessionID: "child"})
	d.RegisterDelegationParent("other-child", DelegationParent{ParentSessionID: "left-behind"})

	require.False(t, d.wakeAllowed("live"), "nothing wakes before a session is reported")

	d.SetLiveSession("live")
	require.True(t, d.wakeAllowed("live"))
	require.True(t, d.wakeAllowed("child"))
	require.True(t, d.wakeAllowed("grandchild"))
	require.False(t, d.wakeAllowed("left-behind"))
	require.False(t, d.wakeAllowed("other-child"), "a delegation of a session left behind is left behind too")
	require.False(t, d.wakeAllowed("unknown-session"))

	// A registration cycle must answer, not hang.
	d.RegisterDelegationParent("a", DelegationParent{ParentSessionID: "b"})
	d.RegisterDelegationParent("b", DelegationParent{ParentSessionID: "a"})
	require.False(t, d.wakeAllowed("a"))
}

// TestDeliverTaskCompletion_CancelledDelegationReportsToItsParentInstead
// is the last-resort rule for a report with nowhere to go.
//
// A cancelled delegation's session is the one delivery target with
// nobody behind it: no person will type into it and no turn will ever
// start there again, so a report left in its inbox is a report thrown
// away. That is not a hypothetical — a grandchild delegation once worked
// for nine minutes after its own parent had been cancelled, filed its
// report into that parent's dead inbox, and the session actually waiting
// for the work sat idle until morning. So the report goes one level up,
// labeled with the delegation that never read it, because "your
// delegation was cancelled and here is what its child managed to do" is
// a different event from an ordinary report.
func TestDeliverTaskCompletion_CancelledDelegationReportsToItsParentInstead(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &promptRecordingModel{text: "done"}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	person, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), person.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier answer"}},
	})
	require.NoError(t, err)

	dead, err := env.sessions.Create(t.Context(), "the cancelled delegation")
	require.NoError(t, err)
	sa.RegisterDelegationParent(dead.ID, DelegationParent{
		Parent:          sa,
		ParentSessionID: person.ID,
		DelegationID:    "task-1",
		Kind:            "task",
		Name:            "developer-junior",
	})
	sa.SetLiveSession(person.ID)
	// The delegation is cancelled: from here nothing will ever run in
	// its session again.
	sa.Cancel(dead.ID)

	sa.DeliverTaskCompletion(t.Context(), dead.ID, testCompletion("nine minutes of work"))

	require.Eventually(t, func() bool { return model.count() > 0 }, 2*time.Second, 5*time.Millisecond,
		"a report addressed to a cancelled delegation must reach the nearest session still working")

	prompts := model.snapshotPrompts()
	require.Len(t, prompts, 1)
	require.True(t, promptContains(prompts[0], "nine minutes of work"),
		"the work itself must survive the redirect")
	require.True(t, promptContains(prompts[0], "developer-junior"),
		"and must say which cancelled delegation it was meant for")
	require.Empty(t, sa.drainCompletionsForStep(dead.ID),
		"nothing may be left behind in the dead session's inbox")
}

// TestDeliverTaskCompletion_CancelledPersonSessionStillWaits is the
// boundary of the rule above. A person's session is never over — they
// pressed Esc, and they will be back — so its inbox is a queue, not a
// dead end, and a report must stay in it rather than be handed to
// somebody else.
func TestDeliverTaskCompletion_CancelledPersonSessionStillWaits(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &promptRecordingModel{text: "done"}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	sa.SetLiveSession(sess.ID)
	sa.Cancel(sess.ID)

	sa.DeliverTaskCompletion(t.Context(), sess.ID, testCompletion("waits-for-the-person"))

	time.Sleep(50 * time.Millisecond)
	require.Zero(t, model.count(), "a session the person canceled must not be woken")
	require.Len(t, sa.drainCompletionsForStep(sess.ID), 1,
		"and the report must be waiting for them, not redirected anywhere")
}

// TestSetLiveSession_WakesWhatCouldNotBeWokenBefore is the other half of
// TestDeliverTaskCompletion_OtherSessionIsNeverWoken: a report that was
// correctly refused a wake while its session was not the live one must
// be taken up the moment it becomes live, not left for the person to
// unstick by typing.
//
// From the bug it was written for: a delegation failed on a provider
// stream error while its parent was not the session on screen, the
// report was enqueued with nobody to start a turn for it, and the
// session then sat visibly dead for five minutes - the parent had
// nothing else in flight whose exit could run wakeFromInboxIfIdle, so
// what finally moved it was an unrelated idle-summarize teardown.
func TestSetLiveSession_WakesWhatCouldNotBeWokenBefore(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &promptRecordingModel{text: "done"}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier answer"}},
	})
	require.NoError(t, err)

	// The person is working somewhere else entirely when the delegation
	// fails, so the report is enqueued and nothing starts.
	sa.SetLiveSession("a-different-session")
	failed := testCompletion("")
	failed.Status = "failed"
	failed.Error = "stall-marker-text"
	sa.DeliverTaskCompletion(t.Context(), sess.ID, failed)

	time.Sleep(50 * time.Millisecond)
	require.Zero(t, model.count(), "a session nobody is working in must never start a turn")

	// They open the session the report was addressed to.
	sa.SetLiveSession(sess.ID)

	require.Eventually(t, func() bool { return model.count() > 0 }, 2*time.Second, 5*time.Millisecond,
		"opening the session must take up the report waiting in it")
	require.Empty(t, sa.drainCompletionsForStep(sess.ID), "the report must have been consumed by that turn")

	prompts := model.snapshotPrompts()
	require.Len(t, prompts, 1, "exactly one turn must have reached the model")
	require.True(t, promptContains(prompts[0], "stall-marker-text"),
		"the failure the delegation reported must be what that turn carries")
}

// TestFoldCompletions_ReportStaysVisibleForLaterSteps is the regression
// test for a parent dispatching the same delegate twice for work already
// reported on. A completion used to be handed to exactly one provider
// request: the fantasy loop rebuilds every step's prompt from the turn's
// initial prompt plus its own response messages, so a message an earlier
// PrepareStep appended was gone by the next step, and persisted history
// kept only the dispatch tool's "started ... its result will follow
// separately". A parent that did not act on the report in that one step
// had no record the delegate had ever reported.
func TestFoldCompletions_ReportStaysVisibleForLaterSteps(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	sa := testSessionAgent(env, &toolCallThenFinishModel{}, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	sa.enqueueCompletion(sess.ID, TaskCompletion{
		DelegationID: "d1",
		Kind:         "task",
		Status:       "completed",
		ResultText:   "report-marker-text",
	})

	turn := &runTurn{agent: sa, call: SessionAgentCall{SessionID: sess.ID}}

	step0, folded, err := turn.foldCompletions(t.Context(), nil, 0)
	require.NoError(t, err)
	require.Len(t, folded, 1)
	require.Len(t, step0, 1)
	require.Contains(t, completionMessageText(t, fantasy.Prompt(step0), "report-marker-text"), "report-marker-text")

	// The next step starts from the same rebuilt prompt this one did -
	// nil here, exactly as fantasy hands it over with nothing of the
	// previous step's PrepareStep in it.
	step1, foldedAgain, err := turn.foldCompletions(t.Context(), nil, 1)
	require.NoError(t, err)
	require.Empty(t, foldedAgain, "nothing new was waiting in the inbox")
	require.Len(t, step1, 1, "the report the previous step folded is handed to this step too")
	require.Contains(t, completionMessageText(t, fantasy.Prompt(step1), "report-marker-text"), "report-marker-text")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	person, reports := countCompletionMessages(msgs, "report-marker-text")
	require.Zero(t, person, "a report is never attributed to the person")
	require.Equal(t, 1, reports, "the report is durable session history, written exactly once")
}

// TestRequeuePendingCompletions_TakesItsMessagesBack covers the other
// half of that stickiness: a batch the step failed on goes back to the
// inbox, so its message must leave the turn's folded set with it -
// otherwise the model sees the same report twice, once from the folded
// set and once from the requeued batch being folded again.
func TestRequeuePendingCompletions_TakesItsMessagesBack(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	sa := testSessionAgent(env, &toolCallThenFinishModel{}, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	sa.enqueueCompletion(sess.ID, TaskCompletion{
		DelegationID: "d1",
		Kind:         "task",
		Status:       "completed",
		ResultText:   "requeue-marker-text",
	})

	turn := &runTurn{agent: sa, call: SessionAgentCall{SessionID: sess.ID}}
	messages, folded, err := turn.foldCompletions(t.Context(), nil, 0)
	require.NoError(t, err)
	require.Len(t, folded, 1)
	require.Len(t, messages, 1)

	turn.pendingCompletions = folded
	turn.requeuePendingCompletions()
	require.Empty(t, turn.foldedCompletions, "the requeued batch takes its message with it")

	next, refolded, err := turn.foldCompletions(t.Context(), nil, 1)
	require.NoError(t, err)
	require.Len(t, refolded, 1, "the batch went back to the inbox and is folded again")
	require.Len(t, next, 1, "and it reaches the model exactly once, not twice")
}
