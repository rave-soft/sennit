package agent

import (
	"net/http"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestClassifyStreamError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  *fantasy.ProviderError
		want streamErrorClass
	}{
		{
			name: "nil provider error",
			err:  nil,
			want: classGenericProviderError,
		},
		{
			name: "copilot's exact original wording",
			err:  &fantasy.ProviderError{Message: "The requested model is not supported."},
			want: classModelNotEnabled,
		},
		{
			name: "copilot rewording the same condition",
			err:  &fantasy.ProviderError{Message: "This model is not enabled for your account."},
			want: classModelNotEnabled,
		},
		{
			name: "different casing and punctuation",
			err:  &fantasy.ProviderError{Message: "REQUESTED MODEL IS NOT SUPPORTED - contact your admin"},
			want: classModelNotEnabled,
		},
		{
			// The status code decides, not the prose: a provider whose
			// 429 body says nothing useful still classifies.
			name: "429 with a message",
			err:  &fantasy.ProviderError{StatusCode: http.StatusTooManyRequests, Message: "rate limit exceeded, please retry later"},
			want: classRateLimited,
		},
		{
			name: "429 with an empty body",
			err:  &fantasy.ProviderError{StatusCode: http.StatusTooManyRequests},
			want: classRateLimited,
		},
		{
			// Prose alone is not enough: plenty of 400s mention rate
			// limits without being one.
			name: "rate-limit wording without the status code",
			err:  &fantasy.ProviderError{StatusCode: http.StatusBadRequest, Message: "rate limit exceeded, please retry later"},
			want: classGenericProviderError,
		},
		{
			name: "empty message",
			err:  &fantasy.ProviderError{},
			want: classGenericProviderError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, classifyStreamError(tt.err))
		})
	}
}
