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

// raceInjectModel is a minimal fantasy.LanguageModel that streams a single
// text part followed by a normal finish, invoking onFinish (if set) right
// before the terminal Finish part is yielded. Tests use onFinish to land a
// simulated concurrent dispatch precisely at the point a real one would have
// to win the session: after this run's own Stream call has produced its
// step, but before the caller has processed shouldSummarize.
type raceInjectModel struct {
	text     string
	onFinish func()
}

func (m *raceInjectModel) Provider() string { return "fake" }
func (m *raceInjectModel) Model() string    { return "fake-model" }

func (m *raceInjectModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: m.text}},
		FinishReason: fantasy.FinishReasonStop,
	}, nil
}

func (m *raceInjectModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
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
		if m.onFinish != nil {
			m.onFinish()
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *raceInjectModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *raceInjectModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// TestRun_AutoSummarizeDoesNotClobberConcurrentActiveRequest covers the
// review finding (2.3) that the auto-summarize branch released its
// activeRequests entry with an unconditional Del, unlike every other
// cleanup path in Run/Summarize, which use CompareAndDelete against the
// run's own *activeCancel. An unconditional Del there can erase a
// different, newer run's entry that won the session in the busy window
// this release opens (Run stops looking busy for the moment between this
// release and Summarize's own busy check / registration).
//
// A context window of 1 forces the StopWhen auto-summarize condition to
// fire after the first step regardless of actual token usage. onFinish
// fires after the model has produced its step but before Run processes
// shouldSummarize, installing a second *activeCancel directly - exactly
// what a real concurrent dispatch's Run would do via activeRequests.Set
// had it raced into the same window. The real race window (between the
// Del/CompareAndDelete call and Summarize's own busy check) is a few
// non-blocking statements wide with no test seam to hook without adding
// production code solely for testing, so this drives the same map
// mutation through the real Run call instead of trying to land two actual
// goroutines in that window.
func TestRun_AutoSummarizeDoesNotClobberConcurrentActiveRequest(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	var racerCanceled atomic.Bool
	racer := &activeCancel{cancel: func() { racerCanceled.Store(true) }}

	model := &raceInjectModel{text: "done"}
	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel: Model{
			Model:      model,
			CatwalkCfg: catwalk.Model{ContextWindow: 1, DefaultMaxTokens: 10000},
		},
		SmallModel: Model{
			Model:      fastModel{},
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000},
		},
		SystemPrompt: "system",
		Sessions:     env.sessions,
		Messages:     env.messages,
	}).(*sessionAgent)

	model.onFinish = func() {
		sa.activeRequests.Set(sess.ID, racer)
	}

	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "hello",
	})
	// Summarize's own busy check observes the racer's entry and bails;
	// this is the visible symptom that the racer's entry survived the
	// shouldSummarize cleanup instead of being erased.
	require.ErrorIs(t, err, ErrSessionBusy)

	got, ok := sa.activeRequests.Get(sess.ID)
	require.True(t, ok, "the concurrently-installed entry must survive this run's cleanup")
	require.Same(t, racer, got, "this run's cleanup must not replace a newer run's entry")
	require.False(t, racerCanceled.Load(), "this run's cleanup must not cancel a newer run's context")
}
