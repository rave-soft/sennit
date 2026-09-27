package cmd

import (
	"fmt"
	"io"
	"net"

	"github.com/rave-soft/sennit/internal/daemon/supervisor"
	"github.com/spf13/cobra"
)

// daemonBridgeCmd is the far end of `sennit attach ssh://...`
// (CLIENT-SERVER.md, PR 3.1): invoked over an SSH session by
// internal/transport.SSHDialer, it ensures this project's daemon is
// running on this machine and bridges the SSH session's stdio to the
// daemon's unix socket byte-for-byte, so the gRPC client on the far end
// of the SSH pipe talks to the daemon as if it had dialed the socket
// itself.
//
// Hidden: a person never types this by hand, only a remote sennit does
// (via `ssh host -- sennit daemon bridge --cwd <path>`).
//
// Stdout carries ONLY the bridged bytes -- gRPC's wire protocol, nothing
// else. Every diagnostic (EnsureRunning's own version-skew warning, any
// error this command itself reports) goes to stderr, never stdout: see
// runBridgeStreams's own doc comment and TestDaemonBridge_StdoutPurity.
var daemonBridgeCmd = &cobra.Command{
	Use:    "bridge",
	Short:  "Bridge stdio to this project's daemon socket (used internally over SSH)",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return err
		}
		debug, _ := cmd.Flags().GetBool("debug")
		dataDir, _ := cmd.Flags().GetString("data-dir")
		ctx := cmd.Context()

		socketPath, warning, err := supervisor.EnsureRunning(ctx, cwd, supervisor.Options{DataDir: dataDir, Debug: debug})
		if err != nil {
			return fmt.Errorf("daemon bridge: %w", err)
		}
		if warning != "" {
			fmt.Fprintln(cmd.ErrOrStderr(), warning)
		}

		conn, err := net.Dial("unix", socketPath)
		if err != nil {
			return fmt.Errorf("daemon bridge: dialing %s: %w", socketPath, err)
		}
		defer conn.Close()

		return runBridgeStreams(cmd.InOrStdin(), cmd.OutOrStdout(), conn)
	},
}

func init() {
	daemonCmd.AddCommand(daemonBridgeCmd)
}

// halfCloser is the subset of net.Conn a bridged connection needs to
// signal "no more data coming from me" without cutting the other
// direction -- *net.UnixConn satisfies it; a test's in-memory pipe pair
// does too when it chooses to.
type halfCloser interface {
	CloseWrite() error
}

// runBridgeStreams copies stdin -> conn and conn -> stdout concurrently
// until both directions have seen EOF (or one side errors), then
// returns. It never writes anything to stdout beyond what it reads from
// conn: the whole point of this command is that stdout IS the gRPC byte
// stream, so a stray log line or progress message here would corrupt
// every frame after it -- see this file's own doc comment on
// daemonBridgeCmd.
//
// Once stdin reaches EOF (the SSH session's local side closed its
// stdin, or `sennit attach` exited), conn's write half is closed via
// CloseWrite when available, so the daemon sees a clean half-close
// instead of the bridge process just going silent; the read direction
// (conn -> stdout) keeps running until the daemon itself closes its end.
func runBridgeStreams(stdin io.Reader, stdout io.Writer, conn net.Conn) error {
	errCh := make(chan error, 2)

	go func() {
		_, err := io.Copy(conn, stdin)
		if hc, ok := conn.(halfCloser); ok {
			_ = hc.CloseWrite()
		}
		errCh <- err
	}()
	go func() {
		_, err := io.Copy(stdout, conn)
		errCh <- err
	}()

	var firstErr error
	for range 2 {
		if err := <-errCh; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
