package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rave-soft/sennit/internal/daemon"
	"github.com/spf13/cobra"
)

// daemonCmd groups the client/server-mode subcommands (CLIENT-SERVER.md,
// PR 2). Only `daemon run` exists so far (PR 2.1); it is hidden from
// `sennit --help` until the rest of the surface (status/stop/restart/
// logs, the supervisor that auto-starts it) lands, since a bare `daemon
// run` with nothing to attach to it is not yet a feature end users
// should reach for.
var daemonCmd = &cobra.Command{
	Use:    "daemon",
	Short:  "Run sennit's backend as a headless, client/server daemon",
	Hidden: true,
}

var daemonRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Bootstrap this project's backend and serve it over a unix socket",
	RunE: func(cmd *cobra.Command, args []string) error {
		debug, _ := cmd.Flags().GetBool("debug")
		yolo, _ := cmd.Flags().GetBool("yolo")
		channels, _ := cmd.Flags().GetStringSlice("channels")
		dataDir, _ := cmd.Flags().GetString("data-dir")
		trustProject, _ := cmd.Flags().GetBool("trust-project")

		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return err
		}

		// SIGTERM alongside SIGINT: a supervisor manages this process by
		// pid and stops it the normal way processes are stopped, not
		// necessarily with Ctrl-C's signal. Both trigger the same
		// graceful path daemon.Run already runs on ctx cancellation.
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		err = daemon.Run(ctx, cwd, daemon.Options{
			DataDir:      dataDir,
			Debug:        debug,
			YOLO:         yolo,
			Channels:     channels,
			TrustProject: trustProject,
		})
		if err != nil {
			return fmt.Errorf("daemon run: %w", err)
		}
		return nil
	},
}

func init() {
	daemonRunCmd.Flags().BoolP("yolo", "y", false, "Automatically accept all permissions (dangerous mode)")
	daemonCmd.AddCommand(daemonRunCmd)
	rootCmd.AddCommand(daemonCmd)
}
