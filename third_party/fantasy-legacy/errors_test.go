package fantasy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsTransientStreamErrorClassifiesByType(t *testing.T) {
	assert.True(t, IsTransientStreamError("overloaded_error", ""))
	assert.True(t, IsTransientStreamError("server_error", "something broke"))
	assert.False(t, IsTransientStreamError("invalid_request_error", "model not found"))
}

// An untyped envelope is what the ChatGPT/Codex backend sends when it sheds
// load: no "type", no "code", only the message. Without the message fallback
// the retry middleware treats it as a permanent failure and the turn dies on
// the first attempt.
func TestIsTransientStreamErrorClassifiesUntypedOverloadByMessage(t *testing.T) {
	require.True(t, IsTransientStreamError("", "Our servers are currently overloaded. Please try again later."))
	assert.True(t, IsTransientStreamError("", "Service Unavailable"))
	assert.True(t, IsTransientStreamError("", "internal server error"))
	assert.False(t, IsTransientStreamError("", "invalid value for parameter 'model'"))
}

func TestTransientStreamErrorIsRetryable(t *testing.T) {
	err := &ProviderError{
		Title:          "stream error",
		Message:        "Our servers are currently overloaded. Please try again later.",
		TransientError: IsTransientStreamError("", "Our servers are currently overloaded. Please try again later."),
	}
	assert.True(t, err.IsRetryable(), "an overloaded provider must be retried, not surfaced as a dead turn")
}
