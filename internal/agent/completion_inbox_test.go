package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// twoStepToolModel streams a tool call ("hold") on its first step, then a
// plain text finish on its second, recording the full Prompt the second
// step's Stream call received. Tests use it to inspect exactly what a
// step folded in, the same way continuationRaceModel (summarize_race_
// test.go) drives a tool-call-then-continuation turn, but capturing the
// step's prompt instead of racing summarize.
type twoStepToolModel struct {
	streams atomic.Int32

	mu          sync.Mutex
	step2Prompt fantasy.Prompt
}

func (m *twoStepToolModel) Provider() string { return "fake" }
func (m *twoStepToolModel) Model() string    { return "fake-model" }

func (m *twoStepToolModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{FinishReason: fantasy.FinishReasonStop}, nil
}

func (m *twoStepToolModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	n := m.streams.Add(1)
	return func(yield func(fantasy.StreamPart) bool) {
		switch n {
		case 1:
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: "tool", ToolCallName: "hold"}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: "tool", Delta: `{}`}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: "tool"}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "tool", ToolCallName: "hold", ToolCallInput: `{}`})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
		default:
			m.mu.Lock()
			m.step2Prompt = call.Prompt
			m.mu.Unlock()
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "done"}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}
	}, nil
}

func (m *twoStepToolModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *twoStepToolModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *twoStepToolModel) lastStep2Prompt() fantasy.Prompt {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.step2Prompt
}

// TestPrepareStep_CompletionDeliveredBeforeSteering proves the ordering
// half of the completion-inbox contract: a completion raised while the
// parent turn is active (mid-turn, between two steps of the same Run)
// is seen by the model on the very next step, ahead of a steering
// follow-up queued at the same time - and delivered as a system
// message, never dressed as something the user typed.
//
// twoStepToolModel's "hold" tool is gated so the test can land both
// events in the window between step 1 finishing (the tool call) and
// step 2's PrepareStep running (which fires only once the tool
// returns) - exactly the "turn is active" window the contract
// describes.
func TestPrepareStep_CompletionDeliveredBeforeSteering(t *testing.T) {
	t.Parallel()
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

	// Land the completion and the steering follow-up "at the same time":
	// both are queued before step 2's PrepareStep has run. The session is
	// busy here (mid step 1, inside the gated tool call), so this only
	// enqueues - it does not attempt to auto-wake a continuation.
	sa.DeliverTaskCompletion(t.Context(), sess.ID, TaskCompletion{
		DelegationID:   "task-1",
		Kind:           "task",
		Name:           "task-name",
		Goal:           "background goal",
		Status:         "completed",
		ChildSessionID: "child-session",
		ResultText:     "background-task-finished-first",
	})
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "steering-message-second"})

	close(gate)

	select {
	case runErr := <-runDone:
		require.NoError(t, runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("run never completed step 2")
	}

	prompt := model.lastStep2Prompt()
	require.NotEmpty(t, prompt, "step 2 must have received a prompt")

	completionIdx, steeringIdx := -1, -1
	var completionRole fantasy.MessageRole
	for i, msg := range prompt {
		for _, part := range msg.Content {
			text, ok := part.(fantasy.TextPart)
			if !ok {
				continue
			}
			if strings.Contains(text.Text, "background-task-finished-first") {
				completionIdx = i
				completionRole = msg.Role
			}
			if strings.Contains(text.Text, "steering-message-second") {
				steeringIdx = i
			}
		}
	}

	require.NotEqual(t, -1, completionIdx, "the completion must appear in step 2's prompt")
	require.NotEqual(t, -1, steeringIdx, "the steering follow-up must appear in step 2's prompt")
	require.Less(t, completionIdx, steeringIdx,
		"the completion must be folded in ahead of the steering follow-up queued at the same time")
	// User-role, not system-role: see taskCompletionsMessage for why a
	// system-role message here would be silently dropped by some
	// provider adapters once other messages precede it. The text itself
	// (not the wire role) is what keeps the model from mistaking this
	// for user speech - checked separately below.
	require.Equal(t, fantasy.MessageRoleUser, completionRole)
	for i, msg := range prompt {
		if i != completionIdx {
			continue
		}
		for _, part := range msg.Content {
			if text, ok := part.(fantasy.TextPart); ok {
				require.Contains(t, text.Text, "system-generated delegation report",
					"the completion's text must label itself as not user input, since its wire role alone no longer signals that")
			}
		}
	}
}

// promptRecordingModel is a plain single-step text-finishing model that
// records the Prompt handed to every non-title Stream call, so a test
// can inspect exactly what a given run's step saw.
type promptRecordingModel struct {
	text string

	mu      sync.Mutex
	prompts []fantasy.Prompt
}

func (m *promptRecordingModel) Provider() string { return "fake" }
func (m *promptRecordingModel) Model() string    { return "fake-model" }

func (m *promptRecordingModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{FinishReason: fantasy.FinishReasonStop}, nil
}

func (m *promptRecordingModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	m.mu.Lock()
	m.prompts = append(m.prompts, call.Prompt)
	m.mu.Unlock()
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

func (m *promptRecordingModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *promptRecordingModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// count returns how many non-title Stream calls have been recorded so
// far. Locked, unlike reading m.prompts directly - Stream runs on its
// own goroutine relative to whatever's polling this from a test.
func (m *promptRecordingModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.prompts)
}

// snapshotPrompts returns a copy of every prompt recorded so far, for a
// test that needs to inspect their contents once settled rather than
// just count them.
func (m *promptRecordingModel) snapshotPrompts() []fantasy.Prompt {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]fantasy.Prompt(nil), m.prompts...)
}

// occurrences counts how many text parts across every recorded prompt
// contain substr, letting a test tell "delivered once" from "delivered
// twice" without caring which step/message carried it.
func (m *promptRecordingModel) occurrences(substr string) int {
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

// TestPrepareStep_CompletionRequeuedOnStepFailure proves the at-most-
// once half of the contract, mirroring
// TestPrepareStep_FoldPersistFailureRequeuesRemainder's structure for
// steering: a step that errors after draining the completion inbox
// (here, because the assistant message create that follows the drain
// fails - completions have no create of their own to fail) must not
// lose the completion.
//
// It does not stop there, though: run()'s own exit (see
// wakeFromInboxIfIdle, agent.go) re-checks the inbox once the failed
// call's session goes idle again and, finding the just-requeued
// completion still sitting there, auto-retries it as a continuation
// turn immediately - the same mechanism that closes the "busy turn with
// no further step" gap for the wake path. So the requeue does not just
// prevent loss in principle; the completion is actually, automatically
// redelivered, and exactly once - not replayed a second time on top of
// an earlier partial delivery.
func TestPrepareStep_CompletionRequeuedOnStepFailure(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	// Call order through PrepareStep: main user message (1), then (since
	// nothing is queued to fold) the step's assistant message create (2).
	// Failing call 2 fails the step *after* the completion drain above it
	// in PrepareStep, without any create of the completion's own to fail.
	// The auto-retry's own creates (3, 4, ...) all land past failOn, so
	// they succeed.
	failing := &failNthCreate{Service: env.messages, failOn: 2}
	env.messages = failing

	model := &promptRecordingModel{text: "done"}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	completion := TaskCompletion{
		DelegationID:   "task-1",
		Kind:           "task",
		Name:           "task-name",
		Goal:           "background goal",
		Status:         "completed",
		ChildSessionID: "child-session",
		ResultText:     "background-task-result-once",
	}
	// The low-level dispatcher primitive, not sa.DeliverTaskCompletion:
	// DeliverTaskCompletion would itself attempt to wake a continuation
	// immediately (the session is idle), which is not what this test is
	// isolating - it wants the completion sitting in the inbox exactly
	// as if a task had completed while this session last went busy, for
	// PrepareStep's own step-0 drain to pick up.
	sa.enqueueCompletion(sess.ID, completion)

	_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "main"})
	require.Error(t, runErr, "the assistant-message create failure must propagate out of Run")

	// Not lost, and not left to wait indefinitely either: run()'s exit
	// re-checks the inbox once idle and auto-retries.
	require.Eventually(t, func() bool { return model.occurrences("background-task-result-once") > 0 }, 2*time.Second, 5*time.Millisecond,
		"a step that errors after draining must requeue the completion, and the auto-retry must actually deliver it")

	require.Empty(t, sa.drainCompletionsForStep(sess.ID),
		"the auto-retry must have drained the inbox; nothing should be left queued")
	require.Equal(t, 1, model.occurrences("background-task-result-once"),
		"the completion must reach the model exactly once across the failed step and its auto-retry, never twice")
}
