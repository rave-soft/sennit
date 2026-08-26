package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// summarizeResumeModel plays the three requests a turn that trips the
// context window makes: the turn's own step (a tool call, so there is work
// left to resume), the summarize pass, and the resumed turn.
type summarizeResumeModel struct {
	streams atomic.Int64

	mu     sync.Mutex
	prompt []fantasy.Prompt // every non-title call's messages, in order
}

func (*summarizeResumeModel) Model() string    { return "resume-model" }
func (*summarizeResumeModel) Provider() string { return "test" }

func (m *summarizeResumeModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *summarizeResumeModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	m.mu.Lock()
	m.prompt = append(m.prompt, call.Prompt)
	m.mu.Unlock()

	switch m.streams.Add(1) {
	case 1:
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: "tool", ToolCallName: "hold"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: "tool", Delta: `{}`})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: "tool"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "tool", ToolCallName: "hold", ToolCallInput: `{}`})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
		}, nil
	default:
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "done"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}
}

func (m *summarizeResumeModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *summarizeResumeModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *summarizeResumeModel) prompts() []fantasy.Prompt {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]fantasy.Prompt(nil), m.prompt...)
}

func newSummarizeResumeAgent(t *testing.T, model fantasy.LanguageModel) (*sessionAgent, string) {
	t.Helper()
	env := testEnv(t)
	hold := fantasy.NewAgentTool("hold", "hold", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	sa := NewSessionAgent(SessionAgentOptions{
		// A window of 1 puts every turn over the threshold, which is how
		// these tests reach the summarize tail on the first step.
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 1, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
		Tools:    []fantasy.AgentTool{hold},
	}).(*sessionAgent)
	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	return sa, sess.ID
}

// TestRun_SummarizedContinuationResumes is the regression test for an
// agent that stopped dead after summarizing.
//
// A continuation's prompt is a placeholder its own step 0 verifies and
// strips (continuationPromptPlaceholder); finishTurn's summarize tail
// rewrote that prompt into an "interrupted" notice before re-queueing the
// call, and the resumed turn — still flagged Continuation — then failed at
// step 0 with "does not match the expected placeholder text". The whole
// turn errored out, so a session that summarized while a delegation's
// answer was being worked on simply stopped.
func TestRun_SummarizedContinuationResumes(t *testing.T) {
	t.Parallel()

	model := &summarizeResumeModel{}
	sa, sessionID := newSummarizeResumeAgent(t, model)

	_, err := sa.Run(t.Context(), SessionAgentCall{
		SessionID:    sessionID,
		Prompt:       continuationPromptPlaceholder,
		Continuation: true,
	})
	require.NoError(t, err, "a summarized continuation must resume, not fail its next step")
	require.GreaterOrEqual(t, model.streams.Load(), int64(3),
		"the turn, its summarize pass, and a resumed turn after it")

	// And the placeholder stays out of the model's sight on the way back
	// in: what the resumed turn reads is the summary, not a fabricated
	// user line about an "initial user request" that was never one.
	for i, prompt := range model.prompts() {
		for _, msg := range prompt {
			for _, part := range msg.Content {
				text, ok := part.(fantasy.TextPart)
				require.False(t, ok && text.Text == continuationPromptPlaceholder,
					"request %d leaked the continuation placeholder into the prompt", i)
			}
		}
	}
}

// TestRun_SummarizedTurnStillTellsTheModelWhatHappened: an ordinary turn
// has a real prompt to remind the model of, and still does.
func TestRun_SummarizedTurnStillTellsTheModelWhatHappened(t *testing.T) {
	t.Parallel()

	model := &summarizeResumeModel{}
	sa, sessionID := newSummarizeResumeAgent(t, model)

	_, err := sa.Run(t.Context(), SessionAgentCall{SessionID: sessionID, Prompt: "build the thing"})
	require.NoError(t, err)

	prompts := model.prompts()
	require.GreaterOrEqual(t, len(prompts), 3)
	// [0] the turn, [1] its summarize pass, [2] the turn it resumes on.
	resumed := prompts[2]
	last := resumed[len(resumed)-1]
	text, ok := last.Content[0].(fantasy.TextPart)
	require.True(t, ok)
	require.Contains(t, text.Text, "interrupted because it got too long")
	require.Contains(t, text.Text, "build the thing", "the request the summary replaced is named")
}
