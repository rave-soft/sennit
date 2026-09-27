package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/daemon/supervisor"
)

// rootFlagsTestCommand builds the flag set wantsDaemon and setupLocalWorkspace
// read, mirroring trustTestCommand's pattern for the same reason: a fresh
// *cobra.Command rather than the package's singleton rootCmd, so this
// test's flags can't race or leak state with any other test's.
func rootFlagsTestCommand(t *testing.T, cwd string) *cobra.Command {
	t.Helper()
	before, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.Chdir(before)) })

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.Flags().String("cwd", cwd, "")
	cmd.Flags().String("data-dir", "", "")
	cmd.Flags().Bool("debug", false, "")
	cmd.Flags().Bool("yolo", false, "")
	cmd.Flags().Bool("trust-project", false, "")
	cmd.Flags().StringSlice("channels", nil, "")
	cmd.Flags().Bool("daemon", false, "")
	return cmd
}

// TestWantsDaemon_FlagOnly pins that the daemon routing decision reads
// only the --daemon flag: unset (the default) is false, and setting it is
// the only way to make it true. There is no config knob left that can
// flip this.
func TestWantsDaemon_FlagOnly(t *testing.T) {
	t.Parallel()

	cmd := rootFlagsTestCommand(t, t.TempDir())
	require.False(t, wantsDaemon(cmd))

	require.NoError(t, cmd.Flags().Set("daemon", "true"))
	require.True(t, wantsDaemon(cmd))
}

// TestRoot_DefaultTakesInProcessPathWithNoDaemon is the regression test
// for the owner decision that a background daemon starts only when asked
// for explicitly: without --daemon, the root command must take the
// in-process path and must never dial or start a project daemon, even
// when the project config sets options.daemon fields (idle_timeout).
//
// Reintroducing a config-driven (or unconditional) daemon start in
// root.go's RunE -- e.g. making wantsDaemon also consult
// options.daemon.idle_timeout, or dropping the "if wantsDaemon(cmd)"
// guard entirely -- turns this red: setupLocalWorkspace would then never
// run, or a daemon socket would appear for cwd where ProbeRunning must
// see none.
func TestRoot_DefaultTakesInProcessPathWithNoDaemon(t *testing.T) {
	writeGlobalConfig(t)

	cwd := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "sennit.json"), []byte(`{
  "options": {"daemon": {"idle_timeout": "1s"}}
}`), 0o644))

	cmd := rootFlagsTestCommand(t, cwd)
	require.NoError(t, cmd.Flags().Set("trust-project", "true"))
	require.False(t, wantsDaemon(cmd), "no --daemon flag means the in-process path")

	ws, cleanup, err := setupLocalWorkspace(cmd)
	require.NoError(t, err)
	require.NotNil(t, ws)

	// The config's own daemon.idle_timeout is read fine -- it just never
	// influenced whether a daemon runs.
	cs, ok := ws.(interface{ ConfigStore() *config.ConfigStore })
	require.True(t, ok)
	require.Equal(t, time.Second, cs.ConfigStore().Config().Options.Daemon.EffectiveIdleTimeout())

	// Release setupLocalWorkspace's own workspace lock before probing:
	// while it is held, ProbeRunning correctly refuses (ErrTUILocked) --
	// that guards a second sennit from clobbering a live TUI, and is not
	// what this test is about. What this test is about is what comes
	// after: setupLocalWorkspace must never itself have started (or
	// dialed) a daemon for cwd.
	cleanup()
	_, running, err := supervisor.ProbeRunning(t.Context(), cwd, supervisor.Options{})
	require.NoError(t, err)
	require.False(t, running, "no daemon should have been started for cwd")
}
