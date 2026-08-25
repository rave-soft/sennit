package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSummarizePolicyWindow covers the ceiling a session may be held to
// instead of the model's own window. It exists because every step of a turn
// re-sends the whole conversation: a model that will accept 872k tokens
// will happily be handed 872k on each of a hundred steps, and the cheapest
// moment to stop that is before the session ever gets there.
func TestSummarizePolicyWindow(t *testing.T) {
	t.Parallel()

	const modelWindow = 872_000

	for _, tc := range []struct {
		name string
		at   int64
		want int64
	}{
		{"a lower ceiling is the one that counts", 400_000, 400_000},
		{"unset leaves the model's window alone", 0, modelWindow},
		{"a ceiling above the window is not a ceiling", 1_000_000, modelWindow},
		{"a ceiling exactly at the window changes nothing", modelWindow, modelWindow},
		{"a negative value is not a ceiling either", -1, modelWindow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, summarizePolicy{at: tc.at}.window(modelWindow))
		})
	}

	// A model that declares no window is the case auto-summarize skips
	// entirely (see stopOnContextWindow): a local model would otherwise be
	// summarized on its first step. A ceiling must not invent a window
	// where there was none and turn that skip into a trap.
	t.Run("no declared window stays unknown", func(t *testing.T) {
		t.Parallel()
		require.Zero(t, summarizePolicy{at: 400_000}.window(0))
	})
}
