package cmd

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/transport"
	"github.com/rave-soft/sennit/internal/uiprefs"
	"github.com/rave-soft/sennit/internal/version"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/spf13/cobra"
)

// remoteDialerOptions builds the transport.DialerOptions a remote
// connect should use from the --remote-bin/--ssh-opt flags every
// command that accepts a remote target shares (CLIENT-SERVER.md, PR
// 3.1).
func remoteDialerOptions(cmd *cobra.Command) transport.DialerOptions {
	remoteBin, _ := cmd.Flags().GetString("remote-bin")
	sshOpts, _ := cmd.Flags().GetStringSlice("ssh-opt")
	return transport.DialerOptions{RemoteBin: remoteBin, SSHOpts: sshOpts}
}

// remoteTargetFlag reads the --remote flag (rootCmd/psCmd) and parses it
// as an ssh:// target if set. ok is false when the flag was never given
// at all -- the ordinary, local-daemon path.
func remoteTargetFlag(cmd *cobra.Command) (target transport.Target, ok bool, err error) {
	raw, _ := cmd.Flags().GetString("remote")
	if raw == "" {
		return transport.Target{}, false, nil
	}
	target, err = transport.ParseTarget(raw)
	return target, true, err
}

// checkRemoteVersion calls Hello on client and reconciles ProtocolVersion/
// BuildID against this build (CLIENT-SERVER.md, PR 3.1, step 5) -- the
// remote counterpart of supervisor.checkVersion, but without any
// restart: this client has no authority to bounce a daemon on someone
// else's machine. A ProtocolVersion mismatch is a hard error naming both
// versions; a BuildID-only mismatch is a warning the caller prints and
// continues past, same as supervisor's own busy-daemon case.
func checkRemoteVersion(ctx context.Context, client *grpcws.Client) (warning string, err error) {
	hello, err := client.Hello(ctx)
	if err != nil {
		return "", fmt.Errorf("saying hello to the remote daemon: %w", err)
	}
	if hello.ProtocolVersion != grpcws.ProtocolVersion {
		return "", fmt.Errorf(
			"the remote daemon speaks protocol version %d but this client expects %d; update sennit on whichever side is older",
			hello.ProtocolVersion, grpcws.ProtocolVersion,
		)
	}
	if hello.BuildID != version.Commit {
		return fmt.Sprintf(
			"using a remote daemon built as %q (this client is built as %q)",
			hello.BuildID, version.Commit,
		), nil
	}
	return "", nil
}

// connectRemoteWorkspace dials target over SSH (internal/transport) and
// connects a *grpcws.Client to whatever daemon answers on the far end,
// mirroring connectDaemonWorkspace's local-socket counterpart (CLIENT-
// SERVER.md, PR 3.1/3.2). It never spawns anything on this machine --
// the remote sennit itself decides whether to start its project's
// daemon (internal/cmd's own daemonBridgeCmd, run over the SSH session
// this dials).
//
// UI preferences come from this client's own GLOBAL config only
// (config.LoadGlobalData): the project lives on the remote machine, so
// there is no local project config layer to read at all -- the owner
// decision CLIENT-SERVER.md records for remote mode ("удалённо").
func connectRemoteWorkspace(ctx context.Context, target transport.Target, dialerOpts transport.DialerOptions, dataDir string, debug bool) (client *grpcws.Client, prefs uiprefs.Store, cleanup func(), err error) {
	dialOpts := append(grpcws.DefaultClientDialOptions(),
		grpc.WithContextDialer(transport.SSHDialer(target, dialerOpts)),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	conn, err := grpc.NewClient("passthrough:///"+target.String(), dialOpts...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("dialing remote daemon at %s: %w", target, err)
	}
	// grpcws.Remote() is what tells an OAuthFlow/Client it must actually
	// relay a browser redirect (CLIENT-SERVER.md, PR 3.3): this is the
	// one path that reaches a daemon on another machine at all.
	client = grpcws.NewClient(conn, grpcws.Remote())

	connectCtx, cancel := context.WithTimeout(ctx, daemonConnectTimeout)
	defer cancel()

	warning, err := checkRemoteVersion(connectCtx, client)
	if err != nil {
		client.Shutdown()
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("connecting to %s: %w", target, err)
	}
	if warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}

	if err := client.Connect(connectCtx); err != nil {
		client.Shutdown()
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("connecting to remote daemon at %s: %w", target, err)
	}

	cfgStore, err := config.LoadGlobalData(dataDir, debug)
	if err != nil {
		client.Shutdown()
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("loading global config for UI preferences: %w", err)
	}

	cleanup = func() {
		client.Shutdown()
		_ = conn.Close()
	}
	return client, uiPrefsStoreFromConfig(cfgStore), cleanup, nil
}

func init() {
	rootCmd.PersistentFlags().String("remote", "", "Connect to a remote project's daemon over SSH (ssh://[user@]host[:port]/path)")
	rootCmd.PersistentFlags().String("remote-bin", "", "Remote sennit binary name for --remote/attach (default: sennit)")
	rootCmd.PersistentFlags().StringSlice("ssh-opt", nil, "Extra -o option for the ssh connection used by --remote/attach (repeatable)")
}
