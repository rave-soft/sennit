package cmd

import (
	"os"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestVerboseLogWriters_VerboseFlag pins the fix for run.go's verbose
// path: setting --verbose must make Setup mirror to os.Stderr instead of
// run.go calling slog.SetDefault after Setup already installed the file
// handler (which discarded it — see internal/log.Setup's initOnce).
func TestVerboseLogWriters_VerboseFlag(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{Use: "run"}
	cmd.Flags().BoolP("verbose", "v", false, "")
	require.NoError(t, cmd.Flags().Set("verbose", "true"))

	got := verboseLogWriters(cmd)
	require.Len(t, got, 1)
	require.Same(t, os.Stderr, got[0])
}

// TestVerboseLogWriters_NoVerboseFlag covers every other command: none of
// doctor/stat/models/logs/session/gc/import define a "verbose" flag, so
// cmd.Flags().GetBool("verbose") fails and must be treated as false
// rather than propagating its error.
func TestVerboseLogWriters_NoVerboseFlag(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{Use: "doctor"}
	require.Empty(t, verboseLogWriters(cmd))
}

// TestVerboseLogWriters_FlagSetFalse covers `run` without --verbose.
func TestVerboseLogWriters_FlagSetFalse(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{Use: "run"}
	cmd.Flags().BoolP("verbose", "v", false, "")

	require.Empty(t, verboseLogWriters(cmd))
}
