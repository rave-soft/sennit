package cmd

import (
	"context"
	"fmt"

	"github.com/rave-soft/sennit/internal/daemon/supervisor"
	"github.com/rave-soft/sennit/internal/uiprefs"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/spf13/cobra"
)

// attachCmd connects to the daemon already running for this project's
// cwd and opens the TUI against it (CLIENT-SERVER.md, PR 2.3) -- the same
// TUI loop the root command's own daemon mode drives, but attachCmd
// never starts one: a project with no daemon running is an error here,
// where the root command would spawn one under options.daemon=auto.
var attachCmd = &cobra.Command{
	Use:   "attach",
	Short: "Attach to this project's running sennit daemon",
	Long:  "Connect to the sennit daemon already running for this project's directory and open the TUI. Never starts a daemon; use `sennit --daemon` (or set options.daemon=auto) for that.",
	RunE: func(cmd *cobra.Command, args []string) error {
		sessionID, _ := cmd.Flags().GetString("session")
		continueLast, _ := cmd.Flags().GetBool("continue")

		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return err
		}
		debug, _ := cmd.Flags().GetBool("debug")
		dataDir, _ := cmd.Flags().GetString("data-dir")

		client, prefs, cleanup, err := setupAttachWorkspace(cmd.Context(), cwd, dataDir, debug)
		if err != nil {
			return err
		}
		defer cleanup()

		return runDaemonTUI(cmd, client, prefs, sessionID, continueLast)
	},
}

func init() {
	attachCmd.Flags().StringP("session", "s", "", "Continue a previous session by ID")
	attachCmd.Flags().BoolP("continue", "C", false, "Continue the most recent session")
	attachCmd.MarkFlagsMutuallyExclusive("session", "continue")
	rootCmd.AddCommand(attachCmd)
}

// errNoDaemonRunning is returned (wrapped with the project directory) by
// any command that must never spawn a daemon on its own -- attach, ps,
// a plain `sennit run` -- when supervisor.ProbeRunning finds none
// running.
func errNoDaemonRunning(cwd string) error {
	return fmt.Errorf("no daemon running for %s; start one with `sennit --daemon` or set options.daemon=auto in sennitrc", cwd)
}

// setupAttachWorkspace finds (never spawns) the daemon running for cwd's
// project and connects a *grpcws.Client to it, mirroring
// setupDaemonWorkspace's split for the spawn-allowed path. Factored out
// so a test can exercise the connect logic without driving a bubbletea
// program, the same way setupDaemonWorkspace is tested.
func setupAttachWorkspace(ctx context.Context, cwd, dataDir string, debug bool) (client *grpcws.Client, prefs uiprefs.Store, cleanup func(), err error) {
	socketPath, running, err := supervisor.ProbeRunning(ctx, cwd, supervisor.Options{DataDir: dataDir, Debug: debug})
	if err != nil {
		return nil, nil, nil, err
	}
	if !running {
		return nil, nil, nil, errNoDaemonRunning(cwd)
	}
	return connectDaemonWorkspace(ctx, cwd, dataDir, debug, socketPath)
}
