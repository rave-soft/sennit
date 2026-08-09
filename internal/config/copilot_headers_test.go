package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A provider declared without extra_headers decodes with a nil map. Copying
// into it used to panic, which took down the process on the Copilot OAuth
// refresh path.
func TestSetupGitHubCopilotWithNilHeaders(t *testing.T) {
	pc := &ProviderConfig{}
	require.NotPanics(t, pc.SetupGitHubCopilot)
	require.NotEmpty(t, pc.ExtraHeaders, "Copilot headers should be present")
}

func TestSetupGitHubCopilotKeepsExistingHeaders(t *testing.T) {
	pc := &ProviderConfig{ExtraHeaders: map[string]string{"X-Mine": "keep"}}
	pc.SetupGitHubCopilot()

	require.Equal(t, "keep", pc.ExtraHeaders["X-Mine"])
	require.Greater(t, len(pc.ExtraHeaders), 1)
}
