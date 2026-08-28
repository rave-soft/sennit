package fantasy

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rateLimitErr() *ProviderError {
	return &ProviderError{Title: "rate limited", StatusCode: http.StatusTooManyRequests}
}

func authErr() *ProviderError {
	return &ProviderError{Title: "unauthorized", StatusCode: http.StatusUnauthorized}
}

// retryableAuthErr is a 401 that is nonetheless retryable (TransientError
// bypasses the status-code check in IsRetryable). It exists to exercise
// isRateLimitError's status-code discrimination directly: an ordinary 401
// never reaches the retry loop's body at all (see isRetryableError), so a
// test built on plain authErr would pass even if OnRateLimit's 429-only
// gate were broken.
func retryableAuthErr() *ProviderError {
	return &ProviderError{Title: "unauthorized", StatusCode: http.StatusUnauthorized, TransientError: true}
}

// countingFn returns a RetryFn that fails with err for the first n calls and
// succeeds afterward, recording how many times it was invoked.
func countingFn(n int, err *ProviderError) (*int, RetryFn[string]) {
	calls := 0
	return &calls, func() (string, error) {
		calls++
		if calls <= n {
			return "", err
		}
		return "ok", nil
	}
}

func TestRetryOnRateLimitHookFiresOn429(t *testing.T) {
	calls, fn := countingFn(1, rateLimitErr())
	var hookCalls int
	var hookErr *ProviderError

	opts := RetryOptions{
		MaxRetries:     3,
		InitialDelayIn: time.Millisecond,
		BackoffFactor:  2.0,
		OnRateLimit: func(_ context.Context, err *ProviderError) error {
			hookCalls++
			hookErr = err
			return nil
		},
	}

	result, err := retryWithExponentialBackoff(context.Background(), fn, opts, nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", result)
	assert.Equal(t, 1, hookCalls)
	assert.Equal(t, http.StatusTooManyRequests, hookErr.StatusCode)
	assert.Equal(t, 2, *calls)
}

func TestRetryOnRateLimitHookDoesNotFireOn401(t *testing.T) {
	// A plain 401 is not a retryable status (see isRetryableError) and
	// never even reaches the retry loop's body, so that alone would pass
	// trivially. Use a 401 flagged TransientError so it IS retryable, which
	// actually exercises isRateLimitError's 429-only gate.
	calls, fn := countingFn(2, retryableAuthErr())
	var hookCalls int

	opts := RetryOptions{
		MaxRetries:     3,
		InitialDelayIn: time.Millisecond,
		BackoffFactor:  2.0,
		OnRateLimit: func(_ context.Context, _ *ProviderError) error {
			hookCalls++
			return nil
		},
	}

	result, err := retryWithExponentialBackoff(context.Background(), fn, opts, nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", result)
	assert.Equal(t, 0, hookCalls)
	assert.Equal(t, 3, *calls)
}

func TestOnAuthRefreshStillFiresOn401AndNotOn429(t *testing.T) {
	t.Run("401 reaches OnAuthRefresh", func(t *testing.T) {
		// fn fails once with a 401, then succeeds; a successful refresh
		// makes the whole pass restart with a fresh budget, so the caller
		// sees a clean success rather than the original 401.
		calls, fn := countingFn(1, authErr())
		var refreshCalls int

		opts := RetryOptions{
			MaxRetries:     0,
			InitialDelayIn: time.Millisecond,
			BackoffFactor:  2.0,
			OnAuthRefresh: func(_ context.Context, _ *ProviderError) error {
				refreshCalls++
				return nil
			},
		}

		retryFn := RetryWithExponentialBackoffRespectingRetryHeaders[string](opts)
		result, err := retryFn(context.Background(), fn)
		require.NoError(t, err)
		assert.Equal(t, "ok", result)
		assert.Equal(t, 1, refreshCalls)
		assert.Equal(t, 2, *calls)
	})

	t.Run("429 does not reach OnAuthRefresh", func(t *testing.T) {
		_, fn := countingFn(1, rateLimitErr())
		var refreshCalls int

		opts := RetryOptions{
			MaxRetries:     0,
			InitialDelayIn: time.Millisecond,
			BackoffFactor:  2.0,
			OnAuthRefresh: func(_ context.Context, _ *ProviderError) error {
				refreshCalls++
				return nil
			},
		}

		retryFn := RetryWithExponentialBackoffRespectingRetryHeaders[string](opts)
		_, err := retryFn(context.Background(), fn)
		require.Error(t, err)
		assert.Equal(t, 0, refreshCalls)
	})
}

func TestRetryOnRateLimitHookFiresAtMostOncePerPass(t *testing.T) {
	// Three consecutive 429s, budget of 3 retries: the hook is offered only
	// the first one; the second and third fall through to normal backoff.
	calls, fn := countingFn(3, rateLimitErr())
	var hookCalls int

	opts := RetryOptions{
		MaxRetries:     3,
		InitialDelayIn: time.Millisecond,
		BackoffFactor:  2.0,
		OnRateLimit: func(_ context.Context, _ *ProviderError) error {
			hookCalls++
			return nil // pretend rotation always "succeeds"
		},
	}

	result, err := retryWithExponentialBackoff(context.Background(), fn, opts, nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", result)
	assert.Equal(t, 1, hookCalls)
	assert.Equal(t, 4, *calls)
}

func TestRetryOnRateLimitHookNilSkipsDelay(t *testing.T) {
	calls, fn := countingFn(1, rateLimitErr())

	opts := RetryOptions{
		MaxRetries:     3,
		InitialDelayIn: 500 * time.Millisecond, // would dominate elapsed time if not skipped
		BackoffFactor:  2.0,
		OnRateLimit: func(_ context.Context, _ *ProviderError) error {
			return nil
		},
	}

	start := time.Now()
	result, err := retryWithExponentialBackoff(context.Background(), fn, opts, nil)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Equal(t, "ok", result)
	assert.Equal(t, 2, *calls)
	assert.Less(t, elapsed, 250*time.Millisecond, "hook-handled attempt should not wait out the backoff delay")
}

func TestRetryOnRateLimitHookErrorLeavesOriginal429Chain(t *testing.T) {
	// MaxRetries: 1 lets the hook run once (on the first 429) and error out,
	// after which the budget is still exhausted by the normal backoff path,
	// producing a *RetryError whose chain is the original 429s — not the
	// hook's error.
	sentinel := rateLimitErr()
	fn := func() (string, error) { return "", sentinel }
	rotationErr := errors.New("no accounts left")

	opts := RetryOptions{
		MaxRetries:     1,
		InitialDelayIn: time.Millisecond,
		BackoffFactor:  2.0,
		OnRateLimit: func(_ context.Context, _ *ProviderError) error {
			return rotationErr
		},
	}

	_, err := retryWithExponentialBackoff(context.Background(), fn, opts, nil)
	require.Error(t, err)
	var retryErr *RetryError
	require.ErrorAs(t, err, &retryErr)
	for _, e := range retryErr.Errors {
		assert.Same(t, sentinel, e)
	}
	assert.ErrorIs(t, err, sentinel)
	assert.NotErrorIs(t, err, rotationErr)
}

func TestRetryOnRateLimitHookErrorFallsThroughToNormalBackoff(t *testing.T) {
	// With a retry budget, a hook error must not stop the retry pass: the
	// attempt proceeds through the normal backoff path (OnRetry still
	// fires) and the request is retried until it succeeds or the budget is
	// spent.
	calls, fn := countingFn(1, rateLimitErr())
	var onRetryCalls int

	opts := RetryOptions{
		MaxRetries:     3,
		InitialDelayIn: time.Millisecond,
		BackoffFactor:  2.0,
		OnRetry: func(_ *ProviderError, _ time.Duration) {
			onRetryCalls++
		},
		OnRateLimit: func(_ context.Context, _ *ProviderError) error {
			return errors.New("rotation failed")
		},
	}

	result, err := retryWithExponentialBackoff(context.Background(), fn, opts, nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", result)
	assert.Equal(t, 2, *calls)
	assert.Equal(t, 1, onRetryCalls, "OnRetry must still fire for the hook-handled attempt when the hook errors")
}

func TestRetryOnRetryDoesNotFireForHookHandledAttempt(t *testing.T) {
	calls, fn := countingFn(1, rateLimitErr())
	var onRetryCalls int

	opts := RetryOptions{
		MaxRetries:     3,
		InitialDelayIn: time.Millisecond,
		BackoffFactor:  2.0,
		OnRetry: func(_ *ProviderError, _ time.Duration) {
			onRetryCalls++
		},
		OnRateLimit: func(_ context.Context, _ *ProviderError) error {
			return nil
		},
	}

	result, err := retryWithExponentialBackoff(context.Background(), fn, opts, nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", result)
	assert.Equal(t, 2, *calls)
	assert.Equal(t, 0, onRetryCalls, "OnRetry should not fire when OnRateLimit handled the attempt")
}

func TestRetryOnRateLimitNilHookIsNoOp(t *testing.T) {
	calls, fn := countingFn(1, rateLimitErr())

	opts := RetryOptions{
		MaxRetries:     3,
		InitialDelayIn: time.Millisecond,
		BackoffFactor:  2.0,
		// OnRateLimit intentionally left nil.
	}

	result, err := retryWithExponentialBackoff(context.Background(), fn, opts, nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", result)
	assert.Equal(t, 2, *calls)
}
