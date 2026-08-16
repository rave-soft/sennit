package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestChannelsFlagAvailableOnRunCmd guards against the --channels flag being
// registered as a local root flag (rootCmd.Flags) rather than a persistent
// one. When local, `sennit run --channels server:webhook` fails with
// "unknown flag" because runCmd does not inherit root's local flags. The
// flag must be persistent so non-interactive runs can opt in to channels
// too.
func TestChannelsFlagAvailableOnRunCmd(t *testing.T) {
	t.Parallel()

	// The flag must be parseable on runCmd — not just rootCmd. Persistent
	// flags are inherited by subcommands; local flags are not.
	require.True(t, runCmd.Flags().HasFlags(), "runCmd flags should be accessible")

	flag := runCmd.Flags().Lookup("channels")
	require.NotNil(t, flag, "the --channels flag must be available on `sennit run` (register it as a persistent flag on rootCmd)")
	require.Equal(t, "stringSlice", flag.Value.Type(), "--channels must be a string slice flag")
}

// TestChannelsFlagAvailableOnRootCmd ensures the flag is still present on the
// root command for interactive mode.
func TestChannelsFlagAvailableOnRootCmd(t *testing.T) {
	t.Parallel()

	flag := rootCmd.Flags().Lookup("channels")
	if flag == nil {
		flag = rootCmd.PersistentFlags().Lookup("channels")
	}
	require.NotNil(t, flag, "the --channels flag must be available on `sennit` (rootCmd)")
}

// TestSmallModelFlagRemovedFromRunCmd guards against --small-model
// reappearing on `sennit run`. Helper (small) model selection is fully
// automatic now, so it must not be exposed as a CLI override.
func TestSmallModelFlagRemovedFromRunCmd(t *testing.T) {
	t.Parallel()

	flag := runCmd.Flags().Lookup("small-model")
	require.Nil(t, flag, "--small-model must not be registered on `sennit run`; helper model selection is automatic")
}
