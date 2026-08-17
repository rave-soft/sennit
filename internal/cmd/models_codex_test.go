package cmd

import (
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/stretchr/testify/require"
)

// TestModelsRefreshCmd_CodexRejectedWhenSignedOut: refresh re-reads a list
// that a sign-in created. With no Codex login there is nothing to re-read,
// and the fix is a sign-in, so say that rather than failing on a 401 from
// the backend.
func TestModelsRefreshCmd_CodexRejectedWhenSignedOut(t *testing.T) {
	setupHermeticConfigEnv(t, `{"providers": {}}`)

	testCmd, _, _ := newRefreshTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", t.TempDir()))

	err := refreshCmd.RunE(testCmd, []string{"codex"})
	require.ErrorContains(t, err, "not signed in to Codex")
}

// TestModelsRefreshCmd_CodexSkippedInTheSweepWhenSignedOut: a bare `models
// refresh` must not start reporting a Codex failure to everyone who has
// never signed in to it.
func TestModelsRefreshCmd_CodexSkippedInTheSweepWhenSignedOut(t *testing.T) {
	setupHermeticConfigEnv(t, `{"providers": {}}`)

	testCmd, stdout, stderr := newRefreshTestCmd(t)
	require.NoError(t, testCmd.Flags().Set("cwd", t.TempDir()))

	require.NoError(t, refreshCmd.RunE(testCmd, nil))
	require.Contains(t, stdout.String(), "no custom providers to refresh")
	require.NotContains(t, stderr.String(), "codex")
}

// TestCodexConfigured keys off the OAuth token rather than the provider
// entry: the entry can exist (a proxy_url set by hand, say) with no login
// behind it, and refreshing that would only produce a 401.
func TestCodexConfigured(t *testing.T) {
	setupHermeticConfigEnv(t, `{"providers": {"codex": {"proxy_url": "http://localhost:8080"}}}`)

	cfg, err := config.Init(t.TempDir(), "", false)
	require.NoError(t, err)
	require.False(t, codexConfigured(cfg))
}
