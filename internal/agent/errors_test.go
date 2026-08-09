package agent

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProviderQuotaError_ErrorsAs ensures the error survives wrapping (as
// it would after propagating through coordinator.run and
// workspace.AppWorkspace.AgentRun) so UI callers can still recognize it via
// errors.As and render their own styled message instead of relying on
// ANSI-escaped text baked into persisted message content.
func TestProviderQuotaError_ErrorsAs(t *testing.T) {
	t.Parallel()

	quotaErr := &ProviderQuotaError{
		Provider:    "copilot",
		Model:       "gpt-5",
		SettingsURL: "https://github.com/settings/copilot/features",
	}
	wrapped := fmt.Errorf("run failed: %w", quotaErr)

	var got *ProviderQuotaError
	require.True(t, errors.As(wrapped, &got))
	require.Equal(t, "copilot", got.Provider)
	require.Equal(t, "gpt-5", got.Model)
	require.Equal(t, "https://github.com/settings/copilot/features", got.SettingsURL)
}

func TestTruncateRunes(t *testing.T) {
	t.Parallel()

	require.Equal(t, "hello", truncateRunes("hello", 10))
	require.Equal(t, "hell…", truncateRunes("hello world", 4))
	require.Equal(t, "hello", truncateRunes("hello", 5))
}
