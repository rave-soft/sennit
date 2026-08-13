package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// recordingModel is a fantasy.LanguageModel that records every Stream call it
// serves. The turn's own stream and the detached title stream are told apart
// by isTitleCall.
type recordingModel struct {
	provider string
	name     string

	mu     sync.Mutex
	turns  int
	titles int

	// titleGot is signalled when the title stream arrives, so tests can
	// wait on the detached title goroutine without sleeping.
	titleGot chan struct{}
}

func newRecordingModel(provider, name string) *recordingModel {
	return &recordingModel{
		provider: provider,
		name:     name,
		titleGot: make(chan struct{}, 1),
	}
}

// counts returns how many turn and title streams this model instance served.
func (m *recordingModel) counts() (turns, titles int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.turns, m.titles
}

func (m *recordingModel) Provider() string { return m.provider }
func (m *recordingModel) Model() string    { return m.name }

func (m *recordingModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *recordingModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	title := isTitleCall(call)

	m.mu.Lock()
	if title {
		m.titles++
	} else {
		m.turns++
	}
	m.mu.Unlock()

	if title {
		select {
		case m.titleGot <- struct{}{}:
		default:
		}
		return titleStream()
	}

	return func(yield func(fantasy.StreamPart) bool) {
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

func (m *recordingModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *recordingModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// TestRunAndTitleUseTheSameModel pins the single-model contract: an agent
// without its own Agent.Model serves both the turn and the detached title
// generation from the one model it was built with. The counters live on that
// model instance, so routing titles to any other model would leave it with a
// turn and no title, and the title assertion below would fail.
func TestRunAndTitleUseTheSameModel(t *testing.T) {
	t.Parallel()

	model := newRecordingModel("fake", "fake-model")

	env := testEnv(t)
	sa := testSessionAgent(env, model, "system")

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	result, err := sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "hello",
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	select {
	case <-model.titleGot:
	case <-time.After(10 * time.Second):
		t.Fatal("the detached title generation never reached the agent's model")
	}

	turns, titles := model.counts()
	require.Equal(t, 1, turns, "the turn ran once on the agent's model")
	require.Equal(t, 1, titles, "the title ran on the agent's model, not a second one")
}
