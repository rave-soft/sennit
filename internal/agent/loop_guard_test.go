package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/message"
)

// loopingModel makes the same "hold" call on every request. Once a request
// carries the loop warning, it either keeps looping (recover false) or
// answers with text and ends the turn (recover true).
type loopingModel struct {
	recover bool

	streams atomic.Int64

	mu            sync.Mutex
	warnedRequest int64 // 1-based index of the first request carrying the warning; 0 if none
}

func (*loopingModel) Model() string    { return "looping-model" }
func (*loopingModel) Provider() string { return "test" }

func (m *loopingModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *loopingModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	n := m.streams.Add(1)
	warned := promptCarriesLoopWarning(call.Prompt)
	if warned {
		m.mu.Lock()
		if m.warnedRequest == 0 {
			m.warnedRequest = n
		}
		m.mu.Unlock()
	}
	if warned && m.recover {
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "the file is what it is"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: "tool", ToolCallName: "hold"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: "tool", Delta: `{}`})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: "tool"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "tool", ToolCallName: "hold", ToolCallInput: `{}`})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
	}, nil
}

func (m *loopingModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *loopingModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func promptCarriesLoopWarning(prompt fantasy.Prompt) bool {
	for _, msg := range prompt {
		for _, part := range msg.Content {
			if text, ok := part.(fantasy.TextPart); ok && strings.HasPrefix(text.Text, toolLoopWarningPrefix) {
				return true
			}
		}
	}
	return false
}

func newLoopingAgent(t *testing.T, model fantasy.LanguageModel) (*sessionAgent, fakeEnv, string) {
	t.Helper()
	env := testEnv(t)
	hold := fantasy.NewAgentTool("hold", "hold", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("same result"), nil
	})
	sa := NewSessionAgent(SessionAgentOptions{
		// An unknown context window keeps auto-summarize out of the way.
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
		Tools:    []fantasy.AgentTool{hold},
	}).(*sessionAgent)
	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	return sa, env, sess.ID
}

// TestRun_ToolLoopWarnsThenStops is the regression test for a turn that
// ended silently on a tool-call loop: no log line, no word to the model,
// and a transcript whose last step read as a turn about to continue.
func TestRun_ToolLoopWarnsThenStops(t *testing.T) {
	t.Parallel()

	model := &loopingModel{}
	sa, env, sessionID := newLoopingAgent(t, model)
	logs := captureLogs(t)

	var complete notify.RunComplete
	_, err := sa.Run(t.Context(), SessionAgentCall{
		SessionID:  sessionID,
		RunID:      "loop-run",
		Prompt:     "read the file",
		OnComplete: func(rc notify.RunComplete) { complete = rc },
	})
	require.NoError(t, err)

	// A delegation reads RunComplete to decide its status: an empty
	// success would report the task as done.
	require.Equal(t, "loop-run", complete.RunID)
	require.Contains(t, complete.Error, toolLoopStopTitle)
	require.False(t, complete.Cancelled)

	// Ten identical steps put six in the window: the warning goes out on
	// the eleventh request, and the eleventh repeat stops the turn.
	require.Equal(t, int64(loopDetectionWindowSize+1), model.streams.Load(),
		"the turn must get one step past the warning, and no more")
	require.Equal(t, int64(loopDetectionWindowSize+1), model.warnedRequest,
		"the request right after the loop was detected must carry the warning")

	msgs, err := env.messages.List(t.Context(), sessionID)
	require.NoError(t, err)

	var warnings []message.Message
	var lastAssistant *message.Message
	for i := range msgs {
		switch msgs[i].Role {
		case message.User:
			if strings.HasPrefix(msgs[i].Content().Text, toolLoopWarningPrefix) {
				warnings = append(warnings, msgs[i])
			}
		case message.Assistant:
			lastAssistant = &msgs[i]
		}
	}
	require.Len(t, warnings, 1, "the warning is persisted once")
	require.Equal(t, message.OriginAgent, warnings[0].Origin)

	require.NotNil(t, lastAssistant)
	finish := lastAssistant.FinishPart()
	require.NotNil(t, finish)
	require.Equal(t, message.FinishReasonToolLoop, finish.Reason)
	require.Equal(t, toolLoopStopTitle, finish.Message)
	require.Contains(t, finish.Details, "hold")
	require.True(t, lastAssistant.IsErrorLike(), "the stop must render as a banner")

	sessionField := "session_id=" + sessionID + " "
	require.Len(t, filterLines(logs.Lines(sessionField), "Repeated tool calls detected"), 1)
	require.Len(t, filterLines(logs.Lines(sessionField), "Stopping turn: the model repeated a tool call"), 1)
}

// TestRun_ToolLoopWarningLetsTheModelRecover: a model that leaves the loop
// once warned finishes its turn normally.
func TestRun_ToolLoopWarningLetsTheModelRecover(t *testing.T) {
	t.Parallel()

	model := &loopingModel{recover: true}
	sa, env, sessionID := newLoopingAgent(t, model)

	_, err := sa.Run(t.Context(), SessionAgentCall{SessionID: sessionID, Prompt: "read the file"})
	require.NoError(t, err)
	require.Equal(t, int64(loopDetectionWindowSize+1), model.streams.Load())

	msgs, err := env.messages.List(t.Context(), sessionID)
	require.NoError(t, err)
	last := msgs[len(msgs)-1]
	require.Equal(t, message.Assistant, last.Role)
	require.Equal(t, message.FinishReasonEndTurn, last.FinishReason())
	require.Equal(t, "the file is what it is", last.Content().Text)
}

// TestRun_ToolLoopRecoveryReportsSuccess: a turn whose model left the loop
// after the warning is an ordinary success on its RunComplete.
func TestRun_ToolLoopRecoveryReportsSuccess(t *testing.T) {
	t.Parallel()

	model := &loopingModel{recover: true}
	sa, _, sessionID := newLoopingAgent(t, model)

	var complete notify.RunComplete
	_, err := sa.Run(t.Context(), SessionAgentCall{
		SessionID:  sessionID,
		RunID:      "recover-run",
		Prompt:     "read the file",
		OnComplete: func(rc notify.RunComplete) { complete = rc },
	})
	require.NoError(t, err)
	require.Equal(t, "recover-run", complete.RunID)
	require.Empty(t, complete.Error)
	require.Equal(t, "the file is what it is", complete.Text)
}

func filterLines(lines []string, needle string) []string {
	var out []string
	for _, line := range lines {
		if strings.Contains(line, needle) {
			out = append(out, line)
		}
	}
	return out
}
