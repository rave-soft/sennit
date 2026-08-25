package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/stretchr/testify/require"
)

// TestShellConfigDotSennitrcTakesPrecedence verifies that a project-local
// .sennitrc overrides sennitrc in the same directory on conflicting settings.
func TestShellConfigDotSennitrcTakesPrecedence(t *testing.T) {
	isolated := t.TempDir()
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))

	workDir := t.TempDir()
	dataDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, "sennitrc"),
		[]byte("option notifications bell\n"), 0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, ".sennitrc"),
		[]byte("option notifications osc\n"), 0o644,
	))
	require.NoError(t, config.Trust(workDir))

	store, err := configruntime.Load(workDir, dataDir, false)
	require.NoError(t, err)
	require.Equal(t, "osc", store.Config().Options.Notifications,
		".sennitrc should win over sennitrc")
}
