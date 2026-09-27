package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/daemon"
	"github.com/rave-soft/sennit/internal/daemon/supervisor"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspacelock"
	"github.com/spf13/cobra"
)

// daemonCmd groups the client/server-mode subcommands (CLIENT-SERVER.md,
// PR 2): `daemon run` (hidden -- see its own doc comment) plus the
// status/stop/restart/logs surface a person actually types.
var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage sennit's backend daemon",
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

// daemonLogsPollInterval is how often `daemon logs --follow` re-checks
// its target file for new content -- poll-based rather than an
// inotify-style watch, so it behaves the same on every OS a daemon can
// run on and stays trivially bounded in a test.
const daemonLogsPollInterval = 500 * time.Millisecond

// daemonLogPath returns the process log file a daemon with this PID
// would have opened from inside its own process (config.GlobalLogFile
// embeds os.Getpid() at the time it runs there) -- computed here from a
// PID read out of the workspace lock, for a caller with no live
// connection of its own to ask.
func daemonLogPath(pid int) string {
	return config.LogFileForPID(pid)
}

// daemonStatusReport is `sennit daemon status`'s whole answer, and its
// --json shape.
type daemonStatusReport struct {
	Running         bool   `json:"running"`
	PID             int    `json:"pid,omitempty"`
	Socket          string `json:"socket,omitempty"`
	ProtocolVersion int    `json:"protocol_version,omitempty"`
	Version         string `json:"version,omitempty"`
	BuildID         string `json:"build_id,omitempty"`
	LogPath         string `json:"log_path,omitempty"`
}

// daemonStatus reports on the daemon for cwd's project without ever
// starting one. PID and socket come from the workspace lock; version/
// build come from a live Hello call, so a crashed daemon that left a
// stale lock record reads as not running rather than as whatever it
// happened to answer last.
func daemonStatus(ctx context.Context, cwd, dataDir string, debug bool) (daemonStatusReport, error) {
	_, lockDir, err := daemon.ResolveSocketPath(ctx, cwd, dataDir, debug)
	if err != nil {
		return daemonStatusReport{}, err
	}
	info, ok, err := workspacelock.CurrentOwner(lockDir)
	if err != nil {
		return daemonStatusReport{}, fmt.Errorf("reading workspace lock: %w", err)
	}
	if !ok || info.Mode != workspacelock.ModeDaemon || info.Socket == "" {
		return daemonStatusReport{}, nil
	}
	if !supervisor.ProbeHealthy(ctx, info.Socket, daemonStatusProbeTimeout) {
		return daemonStatusReport{}, nil
	}

	conn, err := supervisor.Dial(info.Socket)
	if err != nil {
		return daemonStatusReport{}, fmt.Errorf("dialing daemon: %w", err)
	}
	defer conn.Close()
	client := grpcws.NewClient(conn)
	defer client.Shutdown()

	hello, err := client.Hello(ctx)
	if err != nil {
		return daemonStatusReport{}, fmt.Errorf("hello: %w", err)
	}

	return daemonStatusReport{
		Running:         true,
		PID:             info.PID,
		Socket:          info.Socket,
		ProtocolVersion: hello.ProtocolVersion,
		Version:         hello.Version,
		BuildID:         hello.BuildID,
		LogPath:         daemonLogPath(info.PID),
	}, nil
}

// daemonStatusProbeTimeout bounds daemonStatus's own health probe, long
// enough for a loaded machine, short enough not to hang `daemon status`
// on a wedged socket.
const daemonStatusProbeTimeout = 2 * time.Second

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report whether this project's sennit daemon is running",
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return err
		}
		debug, _ := cmd.Flags().GetBool("debug")
		dataDir, _ := cmd.Flags().GetString("data-dir")
		jsonOut, _ := cmd.Flags().GetBool("json")

		report, err := daemonStatus(cmd.Context(), cwd, dataDir, debug)
		if err != nil {
			return err
		}

		if jsonOut {
			return emitJSON(cmd.OutOrStdout(), report)
		}
		if !report.Running {
			fmt.Fprintf(cmd.OutOrStdout(), "no daemon running for %s\n", cwd)
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "running (pid %d)\n", report.PID)
		fmt.Fprintf(cmd.OutOrStdout(), "socket: %s\n", report.Socket)
		fmt.Fprintf(cmd.OutOrStdout(), "version: %s (build %s, protocol %d)\n", report.Version, report.BuildID, report.ProtocolVersion)
		fmt.Fprintf(cmd.OutOrStdout(), "log: %s\n", report.LogPath)
		return nil
	},
}

// requestDaemonShutdown asks the daemon at socketPath to stop, connecting
// only long enough to make the request (and, if refused, to describe why
// via collectPS) -- shared by `daemon stop` and `daemon restart`, which
// both need "refuse if busy unless --force" plus the same busy report.
func requestDaemonShutdown(ctx context.Context, cwd, dataDir string, debug bool, socketPath string, force bool) (accepted bool, report psReport, err error) {
	client, _, cleanup, err := connectDaemonWorkspace(ctx, cwd, dataDir, debug, socketPath)
	if err != nil {
		return false, psReport{}, err
	}
	defer cleanup()

	accepted, err = client.RequestShutdown(ctx, !force)
	if err != nil {
		return false, psReport{}, fmt.Errorf("requesting shutdown: %w", err)
	}
	if accepted {
		return true, psReport{}, nil
	}
	report, rerr := collectPS(ctx, client)
	if rerr != nil {
		return false, psReport{}, fmt.Errorf("daemon refused to stop (busy) and its activity could not be read: %w", rerr)
	}
	return false, report, nil
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop this project's sennit daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return err
		}
		debug, _ := cmd.Flags().GetBool("debug")
		dataDir, _ := cmd.Flags().GetString("data-dir")
		force, _ := cmd.Flags().GetBool("force")
		ctx := cmd.Context()

		socketPath, running, err := supervisor.ProbeRunning(ctx, cwd, supervisor.Options{DataDir: dataDir, Debug: debug})
		if err != nil {
			return err
		}
		if !running {
			fmt.Fprintf(cmd.OutOrStdout(), "no daemon running for %s\n", cwd)
			return nil
		}

		accepted, report, err := requestDaemonShutdown(ctx, cwd, dataDir, debug, socketPath, force)
		if err != nil {
			return err
		}
		if !accepted {
			printPS(cmd.OutOrStdout(), report)
			return errors.New("daemon is busy; use --force to stop it anyway")
		}

		if err := supervisor.AwaitGone(ctx, socketPath); err != nil {
			return fmt.Errorf("waiting for daemon to stop: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "daemon stopped")
		return nil
	},
}

// daemonRestartReadyTimeout overrides EnsureRunning's readiness wait for
// daemonRestartCmd's own spawn below; zero (the production default) keeps
// supervisor's own defaultReadyTimeout. Only a test sets this -- see
// daemon_commands_test.go's TestDaemonRestartCmd, which drives this RunE
// directly (as opposed to through a Command hook) and so has no other way
// to widen this specific wait under -race, where the re-exec'd helper
// process this spawns is genuinely slower to become observable.
var daemonRestartReadyTimeout time.Duration

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart this project's sennit daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return err
		}
		debug, _ := cmd.Flags().GetBool("debug")
		dataDir, _ := cmd.Flags().GetString("data-dir")
		force, _ := cmd.Flags().GetBool("force")
		ctx := cmd.Context()

		socketPath, running, err := supervisor.ProbeRunning(ctx, cwd, supervisor.Options{DataDir: dataDir, Debug: debug})
		if err != nil {
			return err
		}
		if running {
			accepted, report, err := requestDaemonShutdown(ctx, cwd, dataDir, debug, socketPath, force)
			if err != nil {
				return err
			}
			if !accepted {
				printPS(cmd.OutOrStdout(), report)
				return errors.New("daemon is busy; use --force to restart it anyway")
			}
			if err := supervisor.AwaitGone(ctx, socketPath); err != nil {
				return fmt.Errorf("waiting for daemon to stop: %w", err)
			}
			// AwaitGone only proves the outgoing daemon's socket has gone
			// quiet, not that its process has actually exited and released
			// the workspace lock (see AwaitWorkspaceLockFree's own doc
			// comment) -- waiting for that here, on its own budget, keeps
			// that teardown time from silently eating into the ReadyTimeout
			// the new daemon gets below.
			if _, lockDir, err := daemon.ResolveSocketPath(ctx, cwd, dataDir, debug); err == nil {
				if err := supervisor.AwaitWorkspaceLockFree(ctx, lockDir); err != nil {
					return fmt.Errorf("waiting for the old daemon to release its workspace lock: %w", err)
				}
			}
		}

		_, warning, err := supervisor.EnsureRunning(ctx, cwd, supervisor.Options{
			DataDir:      dataDir,
			Debug:        debug,
			ReadyTimeout: daemonRestartReadyTimeout,
		})
		if err != nil {
			return err
		}
		if warning != "" {
			fmt.Fprintln(os.Stderr, warning)
		}

		report, err := daemonStatus(ctx, cwd, dataDir, debug)
		if err != nil {
			return err
		}
		if report.Running {
			fmt.Fprintf(cmd.OutOrStdout(), "daemon restarted (pid %d)\n", report.PID)
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "daemon restarted")
		}
		return nil
	},
}

// printLogFile writes path's whole contents to w, prefixed by a header
// naming it -- used for both the daemon's own process log and its
// startup log, which `daemon logs` shows together.
func printLogFile(w io.Writer, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	fmt.Fprintf(w, "==> %s <==\n", path)
	_, err = w.Write(data)
	return err
}

// followFilePoll polls path for content appended since it was opened,
// writing it to w every daemonLogsPollInterval, until ctx is done.
// Poll-based rather than an inotify-style watch, so `daemon logs -f`
// behaves the same on every OS a daemon can run on and a test can bound
// it purely by cancelling ctx.
func followFilePoll(ctx context.Context, w io.Writer, path string) error {
	var offset int64
	if fi, err := os.Stat(path); err == nil {
		offset = fi.Size()
	}
	ticker := time.NewTicker(daemonLogsPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			f, err := os.Open(path)
			if err != nil {
				continue
			}
			if _, err := f.Seek(offset, io.SeekStart); err != nil {
				f.Close()
				continue
			}
			n, _ := io.Copy(w, f)
			offset += n
			f.Close()
		}
	}
}

var daemonLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show this project's sennit daemon log",
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return err
		}
		debug, _ := cmd.Flags().GetBool("debug")
		dataDir, _ := cmd.Flags().GetString("data-dir")
		follow, _ := cmd.Flags().GetBool("follow")
		ctx := cmd.Context()

		_, lockDir, err := daemon.ResolveSocketPath(ctx, cwd, dataDir, debug)
		if err != nil {
			return err
		}
		info, ok, err := workspacelock.CurrentOwner(lockDir)
		if err != nil {
			return fmt.Errorf("reading workspace lock: %w", err)
		}

		var logPath string
		if ok && info.PID != 0 {
			logPath = daemonLogPath(info.PID)
		}
		startupLog := supervisor.StartupLogPath(lockDir)

		out := cmd.OutOrStdout()
		var shown, printErr bool
		if logPath != "" {
			if _, statErr := os.Stat(logPath); statErr == nil {
				shown = true
				if err := printLogFile(out, logPath); err != nil {
					printErr = true
				}
			}
		}
		if _, statErr := os.Stat(startupLog); statErr == nil {
			shown = true
			if err := printLogFile(out, startupLog); err != nil {
				printErr = true
			}
		}
		if printErr {
			return fmt.Errorf("reading daemon log files for %s", cwd)
		}
		if !shown {
			fmt.Fprintf(out, "no log file found for this project's daemon\n")
			return nil
		}

		if !follow {
			return nil
		}
		target := logPath
		if target == "" {
			target = startupLog
		}
		return followFilePoll(ctx, out, target)
	},
}

func init() {
	daemonRunCmd.Hidden = true
	daemonRunCmd.Flags().BoolP("yolo", "y", false, "Automatically accept all permissions (dangerous mode)")
	daemonStatusCmd.Flags().Bool("json", false, "output in JSON format")
	daemonStopCmd.Flags().Bool("force", false, "Stop the daemon even if it is busy")
	daemonRestartCmd.Flags().Bool("force", false, "Restart the daemon even if it is busy")
	daemonLogsCmd.Flags().BoolP("follow", "f", false, "Follow log output")

	daemonCmd.AddCommand(daemonRunCmd, daemonStatusCmd, daemonStopCmd, daemonRestartCmd, daemonLogsCmd)
	rootCmd.AddCommand(daemonCmd)
}
