package brand

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConstants pins every exported constant to its current literal value.
// It is deliberately a tripwire: a future rename must edit this test, not
// slip past it silently.
func TestConstants(t *testing.T) {
	t.Parallel()

	require.Equal(t, "Sennit", Name)
	require.Equal(t, "sennit", Slug)
	require.Equal(t, "SENNIT", EnvName)
	require.Equal(t, "SENNIT_", EnvPrefix)

	require.Equal(t, "rave-soft", Vendor)
	require.Equal(t, "https://github.com/rave-soft/sennit", RepoURL)
	require.Equal(t, "SENNIT", Wordmark)

	require.Equal(t, ".sennit", DataDir)
	require.Equal(t, "sennitrc", ShellConfigFile)
	require.Equal(t, ".sennitrc", HiddenShellConfigFile)
	require.Equal(t, "sennit.json", JSONConfigFile)
	require.Equal(t, ".sennit.json", HiddenJSONConfigFile)
	require.Equal(t, ".sennitignore", IgnoreFile)
	require.Equal(t, "SENNIT.md", ContextFile)
	require.Equal(t, "SENNIT.local.md", ContextFileLocal)

	require.Equal(t, "sennit.db", DBFile)
	require.Equal(t, "sennit.log", LogFile)
	require.Equal(t, "sennit.lock", LockFile)

	require.Equal(t, "sennit://", SkillsURIScheme)
	require.Equal(t, "sennit_info", ToolInfo)
	require.Equal(t, "sennit_logs", ToolLogs)
	require.Equal(t, "sennit.png", IconFile)
}
