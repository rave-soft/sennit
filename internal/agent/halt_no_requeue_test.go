package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// haltThresholdModel plays the two requests a turn that both trips the
// context-window threshold and gets halted by a tool on the same step
// should make: the turn's own step (a tool call whose result carries
// StopTurn), and the summarize pass. A third stream call - the requeued
// continuation this test guards against - would mean the halt was ignored.
type haltThresholdModel struct {
	streams atomic.Int64
}

func (*haltThresholdModel) Model() string    { return "halt-model" }
func (*haltThresholdModel) Provider() string { return "test" }

func (m *haltThresholdModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *haltThresholdModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	switch m.streams.Add(1) {
	case 1:
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: "tool", ToolCallName: "halt"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: "tool", Delta: `{}`})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: "tool"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "tool", ToolCallName: "halt", ToolCallInput: `{}`})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
		}, nil
	default:
		// The summarize pass.
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "summary"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}
}

func (m *haltThresholdModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *haltThresholdModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// TestFinishTurn_HaltedStepDoesNotRequeueContinuation is the regression
// test for G14: a hook Halt (or a permission denial, or a pending
// question) sets ToolResultContent.StopTurn, which onStepFinish already
// reads to rewrite the step's finish reason to EndTurn - but fantasy's own
// StopWhen conditions (stopOnContextWindow included) are evaluated on the
// same step regardless of that halt, so shouldSummarize can trip on
// exactly the step a halt just ended. finishTurn used to look only at
// whether the assistant message still carried tool calls to decide whether
// to requeueContinuation, missing the halt entirely: it summarized the
// context and then silently resumed the "interrupted" turn the halt meant
// to stop - directly contradicting hooked_tool.go's documented "Halt ends
// the whole turn".
func TestFinishTurn_HaltedStepDoesNotRequeueContinuation(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	halt := fantasy.NewAgentTool("halt", "halt", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		resp := fantasy.NewTextResponse("halted")
		resp.StopTurn = true
		return resp, nil
	})

	model := &haltThresholdModel{}
	sa := NewSessionAgent(SessionAgentOptions{
		// A window of 1 puts every turn over the auto-summarize threshold
		// on its first step, the same trick summarize_continuation_test.go
		// uses.
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 1, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
		Tools:    []fantasy.AgentTool{halt},
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "do the thing"})
	require.NoError(t, err)

	require.Equal(t, int64(2), model.streams.Load(),
		"only the halted turn and its summarize pass - a third stream call would be the continuation the halt must suppress")
	require.False(t, sa.IsSessionBusy(sess.ID))
	require.Equal(t, 0, sa.QueuedPrompts(sess.ID))
}
