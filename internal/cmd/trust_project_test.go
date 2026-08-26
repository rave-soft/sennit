package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// trustTestCommand builds the flag set initConfig reads. It also restores
// the working directory: ResolveCwd chdirs into --cwd, and on Windows a
// directory that is the process's own cwd cannot be removed, which turned
// every t.TempDir cleanup here into a failure of a test that had passed.
func trustTestCommand(t *testing.T, project string, trust bool) *cobra.Command {
	t.Helper()
	before, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.Chdir(before)) })

	cmd := &cobra.Command{}
	cmd.Flags().String("cwd", project, "")
	cmd.Flags().String("data-dir", "", "")
	cmd.Flags().Bool("trust-project", trust, "")
	return cmd
}

func TestInitConfig_TrustProjectBeforeLoad(t *testing.T) {
	t.Setenv("SENNIT_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("SENNIT_GLOBAL_DATA", t.TempDir())
	project := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(project, "sennit.json"), []byte(`{"env":{"SENNIT_CMD_TRUST":"enabled"}}`), 0o600))

	_, store, err := initConfig(trustTestCommand(t, project, true), false)
	require.NoError(t, err)
	require.True(t, config.IsTrusted(project))
	require.Equal(t, "enabled", store.Config().Env["SENNIT_CMD_TRUST"])
}

func TestInitConfig_TrustProjectPersists(t *testing.T) {
	t.Setenv("SENNIT_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("SENNIT_GLOBAL_DATA", t.TempDir())
	project := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(project, "sennit.json"), []byte(`{"env":{"SENNIT_CMD_TRUST":"enabled"}}`), 0o600))

	_, _, err := initConfig(trustTestCommand(t, project, true), false)
	require.NoError(t, err)
	_, store, err := initConfig(trustTestCommand(t, project, false), false)
	require.NoError(t, err)
	require.Equal(t, "enabled", store.Config().Env["SENNIT_CMD_TRUST"])
}

func TestInitConfig_TrustProjectMarkerError(t *testing.T) {
	globalFile := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(globalFile, nil, 0o600))
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalFile)
	project := t.TempDir()

	_, _, err := initConfig(trustTestCommand(t, project, true), false)
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to trust project")
}

func TestSetupLocalWorkspace_TrustProjectMarkerError(t *testing.T) {
	globalFile := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(globalFile, nil, 0o600))
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalFile)
	project := t.TempDir()
	cmd := trustTestCommand(t, project, true)
	cmd.Flags().Bool("debug", false, "")
	cmd.Flags().Bool("yolo", false, "")
	cmd.Flags().StringSlice("channels", nil, "")

	_, _, err := setupLocalWorkspace(cmd)
	require.ErrorContains(t, err, "failed to trust project")
}

func TestTrustProjectFlagIsPersistent(t *testing.T) {
	require.NotNil(t, rootCmd.PersistentFlags().Lookup("trust-project"))
	require.NotNil(t, runCmd.InheritedFlags().Lookup("trust-project"))
}
