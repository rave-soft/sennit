package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// scriptedToolModel records every non-title prompt it receives and answers
// the first toolSteps of them with a call to toolName, then finishes with
// plain text.
type scriptedToolModel struct {
	toolName  string
	toolSteps int

	mu      sync.Mutex
	prompts []fantasy.Prompt
}

func (m *scriptedToolModel) Provider() string { return "fake" }
func (m *scriptedToolModel) Model() string    { return "fake-model" }

func (m *scriptedToolModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{FinishReason: fantasy.FinishReasonStop}, nil
}

func (m *scriptedToolModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	m.mu.Lock()
	m.prompts = append(m.prompts, cloneFantasyMessages(call.Prompt))
	n := len(m.prompts)
	m.mu.Unlock()
	return func(yield func(fantasy.StreamPart) bool) {
		if n <= m.toolSteps {
			id := "tool-" + string(rune('0'+n))
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: id, ToolCallName: m.toolName}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: id, Delta: `{}`}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: id}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: m.toolName, ToolCallInput: `{}`})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: "done"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *scriptedToolModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *scriptedToolModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *scriptedToolModel) snapshot() []fantasy.Prompt {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]fantasy.Prompt(nil), m.prompts...)
}

// promptIndexOf returns the index of every message in prompt with a text
// part containing marker.
func promptIndexOf(prompt fantasy.Prompt, marker string) []int {
	var at []int
	for i, msg := range prompt {
		for _, part := range msg.Content {
			if text, ok := part.(fantasy.TextPart); ok && strings.Contains(text.Text, marker) {
				at = append(at, i)
				break
			}
		}
	}
	return at
}

// firstToolCallIndex returns the index of the first assistant message in
// prompt that carries a tool call, or -1.
func firstToolCallIndex(prompt fantasy.Prompt) int {
	for i, msg := range prompt {
		if msg.Role != fantasy.MessageRoleAssistant {
			continue
		}
		for _, part := range msg.Content {
			if _, ok := part.(fantasy.ToolCallPart); ok {
				return i
			}
		}
	}
	return -1
}

func noopTool(name string, onCall func()) fantasy.AgentTool {
	return fantasy.NewAgentTool(name, "does nothing", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		if onCall != nil {
			onCall()
		}
		return fantasy.NewTextResponse("ok"), nil
	})
}

// TestContinuation_ReportKeepsItsPlaceAcrossSteps drives a real
// continuation turn of two steps. The report that woke it must reach the
// second step exactly once, before the first step's tool call: appended
// after the tool result, it reads to the model as the same report
// delivered again, and a model that dismisses it as a duplicate ends the
// turn without acting on it. The placeholder must not reach either step.
func TestContinuation_ReportKeepsItsPlaceAcrossSteps(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedToolModel{toolName: "noop", toolSteps: 1}
	sa := testSessionAgent(env, model, "system", noopTool("noop", nil)).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier answer"}},
	})
	require.NoError(t, err)

	sa.enqueueCompletion(sess.ID, testCompletion("order-marker-report"))
	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID:    sess.ID,
		Prompt:       continuationPromptPlaceholder,
		Continuation: true,
	})
	require.NoError(t, err)

	prompts := model.snapshot()
	require.Len(t, prompts, 2, "the continuation runs a tool step and a finishing step")
	for i, prompt := range prompts {
		require.Empty(t, promptIndexOf(prompt, continuationPromptPlaceholder), "step %d must not carry the placeholder", i)
		require.Len(t, promptIndexOf(prompt, "order-marker-report"), 1, "step %d must carry the report exactly once", i)
	}

	step1 := prompts[1]
	report := promptIndexOf(step1, "order-marker-report")[0]
	call := firstToolCallIndex(step1[report:])
	require.GreaterOrEqual(t, call, 0, "the report must come before the tool call the model made in response to it")
}

// TestSteering_KeepsItsPlaceAcrossSteps covers the same rebuild for a
// follow-up the person typed while a turn was running: folded into one
// step, it must stay in every later step of that turn, in the place it
// was folded, not vanish once the next step rebuilds its prompt.
func TestSteering_KeepsItsPlaceAcrossSteps(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedToolModel{toolName: "noop", toolSteps: 2}
	var sa *sessionAgent
	var sessionID string
	var once sync.Once
	steer := func() {
		once.Do(func() {
			outcome, _, err := sa.Steer(context.Background(), SessionAgentCall{SessionID: sessionID, Prompt: "order-marker-steer"})
			require.NoError(t, err)
			require.Equal(t, SteerEnqueued, outcome)
		})
	}
	sa = testSessionAgent(env, model, "system", noopTool("noop", steer)).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	sessionID = sess.ID

	done := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "main"})
		done <- runErr
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never finished")
	}

	prompts := model.snapshot()
	require.Len(t, prompts, 3, "two tool steps and a finishing step")
	require.Empty(t, promptIndexOf(prompts[0], "order-marker-steer"))
	for i := 1; i < 3; i++ {
		require.Len(t, promptIndexOf(prompts[i], "order-marker-steer"), 1, "step %d must carry the follow-up exactly once", i)
	}

	// Folded after the first tool result, it stays ahead of the second
	// step's tool call.
	step2 := prompts[2]
	steerAt := promptIndexOf(step2, "order-marker-steer")[0]
	require.Greater(t, steerAt, firstToolCallIndex(step2))
	require.GreaterOrEqual(t, firstToolCallIndex(step2[steerAt:]), 0)
}
