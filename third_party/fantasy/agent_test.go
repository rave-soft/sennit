package fantasy

import (
	"context"
	"iter"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingStreamModel is a LanguageModel whose Stream fails with a given
// error for the first n calls, then succeeds with a minimal finished
// stream. It exists to exercise AgentStreamCall.OnRateLimit/OnAuthRefresh
// end to end (through agent.Stream's real retry wiring), the same way
// retry_test.go exercises RetryOptions.OnRateLimit directly against
// RetryWithExponentialBackoffRespectingRetryHeaders.
type countingStreamModel struct {
	failN int
	err   error
	calls int
}

func (m *countingStreamModel) Generate(context.Context, Call) (*Response, error) {
	m.calls++
	if m.calls <= m.failN {
		return nil, m.err
	}
	return &Response{FinishReason: FinishReasonStop}, nil
}

func (m *countingStreamModel) Stream(context.Context, Call) (StreamResponse, error) {
	m.calls++
	if m.calls <= m.failN {
		return nil, m.err
	}
	return iter.Seq[StreamPart](func(yield func(StreamPart) bool) {
		if !yield(StreamPart{Type: StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(StreamPart{Type: StreamPartTypeTextDelta, ID: "1", Delta: "ok"}) {
			return
		}
		if !yield(StreamPart{Type: StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		yield(StreamPart{Type: StreamPartTypeFinish, FinishReason: FinishReasonStop})
	}), nil
}

func (m *countingStreamModel) GenerateObject(context.Context, ObjectCall) (*ObjectResponse, error) {
	panic("not used by this test")
}

func (m *countingStreamModel) StreamObject(context.Context, ObjectCall) (ObjectStreamResponse, error) {
	panic("not used by this test")
}

func (m *countingStreamModel) Provider() string { return "test" }
func (m *countingStreamModel) Model() string    { return "test-model" }

// TestAgentStreamCallOnRateLimitReaches429NotConfusedWithOnAuthRefresh
// proves the vendored-fork threading added in agent.go (AgentStreamCall ->
// AgentCall -> retryOptions) actually reaches the retry loop: a 429
// fires OnRateLimit and not OnAuthRefresh, a 401 the reverse. This mirrors
// retry_test.go's TestOnAuthRefreshStillFiresOn401AndNotOn429, one level
// up the stack, at the field agent.Stream actually reads.
func TestAgentStreamCallOnRateLimitReaches429NotConfusedWithOnAuthRefresh(t *testing.T) {
	t.Run("429 reaches OnRateLimit, not OnAuthRefresh", func(t *testing.T) {
		model := &countingStreamModel{failN: 1, err: &ProviderError{Title: "rate limited", StatusCode: http.StatusTooManyRequests}}
		a := NewAgent(model)

		var rateLimitCalls, authRefreshCalls int
		result, err := a.Stream(context.Background(), AgentStreamCall{
			Prompt: "hi",
			OnRateLimit: func(context.Context, *ProviderError) error {
				rateLimitCalls++
				return nil
			},
			OnAuthRefresh: func(context.Context, *ProviderError) error {
				authRefreshCalls++
				return nil
			},
		})
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, 1, rateLimitCalls)
		assert.Equal(t, 0, authRefreshCalls)
		assert.Equal(t, 2, model.calls)
	})

	t.Run("401 reaches OnAuthRefresh, not OnRateLimit", func(t *testing.T) {
		model := &countingStreamModel{failN: 1, err: &ProviderError{Title: "unauthorized", StatusCode: http.StatusUnauthorized}}
		a := NewAgent(model)

		var rateLimitCalls, authRefreshCalls int
		result, err := a.Stream(context.Background(), AgentStreamCall{
			Prompt: "hi",
			OnRateLimit: func(context.Context, *ProviderError) error {
				rateLimitCalls++
				return nil
			},
			OnAuthRefresh: func(context.Context, *ProviderError) error {
				authRefreshCalls++
				return nil
			},
		})
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, 0, rateLimitCalls)
		assert.Equal(t, 1, authRefreshCalls)
		assert.Equal(t, 2, model.calls)
	})
}

// TestAgentGenerateOnRateLimitReaches429 covers the non-streaming Generate
// path (agent.Generate's own retryOptions wiring, a separate code path
// from agent.Stream) so both threading sites added for OnRateLimit are
// proven, not just Stream's.
func TestAgentGenerateOnRateLimitReaches429(t *testing.T) {
	model := &countingStreamModel{failN: 1, err: &ProviderError{Title: "rate limited", StatusCode: http.StatusTooManyRequests}}
	a := NewAgent(model)

	var rateLimitCalls int
	result, err := a.Generate(context.Background(), AgentCall{
		Prompt: "hi",
		OnRateLimit: func(context.Context, *ProviderError) error {
			rateLimitCalls++
			return nil
		},
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1, rateLimitCalls)
	assert.Equal(t, 2, model.calls)
}
