package openai

import (
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The ChatGPT/Codex backend sheds load with an error envelope that carries a
// message but no "type" and no "code". The SDK turns any event whose data has
// an "error" key into a *ssestream.StreamError before the event switch sees
// it, so this is the path a Codex overload takes.
func TestStreamErrorOverloadWithoutTypeIsRetryable(t *testing.T) {
	streamErr := &ssestream.StreamError{
		Message: "received error while streaming",
		Event: ssestream.Event{
			Type: "error",
			Data: []byte(`{"error":{"message":"Our servers are currently overloaded. Please try again later."}}`),
		},
	}

	var providerErr *fantasy.ProviderError
	require.True(t, errors.As(toProviderErr(streamErr), &providerErr))
	assert.Equal(t, "Our servers are currently overloaded. Please try again later.", providerErr.Message)
	assert.True(t, providerErr.IsRetryable(), "an overloaded provider must be retried, not surfaced as a dead turn")
}

func TestStreamErrorPermanentFailureIsNotRetryable(t *testing.T) {
	streamErr := &ssestream.StreamError{
		Message: "received error while streaming",
		Event: ssestream.Event{
			Type: "error",
			Data: []byte(`{"error":{"type":"invalid_request_error","message":"Unknown model"}}`),
		},
	}

	var providerErr *fantasy.ProviderError
	require.True(t, errors.As(toProviderErr(streamErr), &providerErr))
	assert.False(t, providerErr.IsRetryable())
}

// A Responses `error` event whose data has no "error" key reaches the event
// switch instead, so it needs the same classification.
func TestResponsesErrorEventOverloadIsRetryable(t *testing.T) {
	var providerErr *fantasy.ProviderError
	err := responsesErrorStreamError("Our servers are currently overloaded. Please try again later.", "")
	require.True(t, errors.As(err, &providerErr))
	assert.True(t, providerErr.IsRetryable())

	require.True(t, errors.As(responsesFailedStreamError("The server had an error", "server_error"), &providerErr))
	assert.True(t, providerErr.IsRetryable())

	require.True(t, errors.As(responsesFailedStreamError("Unsupported tool", "invalid_request_error"), &providerErr))
	assert.False(t, providerErr.IsRetryable())
}
