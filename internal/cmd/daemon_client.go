package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/daemon/supervisor"
	"github.com/rave-soft/sennit/internal/devtools"
	"github.com/rave-soft/sennit/internal/ui/common"
	ui "github.com/rave-soft/sennit/internal/ui/model"
	"github.com/rave-soft/sennit/internal/uiprefs"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/spf13/cobra"
)

// daemonConnectTimeout bounds a fresh gRPC Client's Connect call (Hello +
// Snapshot) once EnsureRunning has already handed back a healthy socket --
// separate from EnsureRunning's own ReadyTimeout, which bounds finding or
// spawning the daemon in the first place.
const daemonConnectTimeout = 30 * time.Second

// runInteractiveDaemon is the daemon-mode counterpart to the root
// command's usual in-process path (setupLocalWorkspace): it finds or
// starts cwd's project daemon, connects a *grpcws.Client to it, and hands
// that client to the TUI exactly where an *appws.AppWorkspace goes today
// (CLIENT-SERVER.md, PR 2.3). Quitting the TUI here never stops a running
// turn -- the daemon keeps it -- so this only shuts down the client's own
// connection, never the daemon.
func runInteractiveDaemon(cmd *cobra.Command, cwd, sessionID string, continueLast bool) error {
	ctx := cmd.Context()
	debug, _ := cmd.Flags().GetBool("debug")
	dataDir, _ := cmd.Flags().GetString("data-dir")

	client, prefs, cleanup, err := setupDaemonWorkspace(ctx, cwd, dataDir, debug, supervisor.Options{})
	if err != nil {
		return err
	}
	defer cleanup()

	return runDaemonTUI(cmd, client, prefs, sessionID, continueLast)
}

// runDaemonTUI drives the TUI against an already-connected daemon client
// -- the tail end of runInteractiveDaemon, factored out so `sennit
// attach` (which connects without ever being allowed to spawn a daemon,
// see supervisor.ProbeRunning) can reach the same TUI loop without
// duplicating it.
func runDaemonTUI(cmd *cobra.Command, client *grpcws.Client, prefs uiprefs.Store, sessionID string, continueLast bool) error {
	ctx := cmd.Context()

	if sessionID != "" {
		sess, err := resolveSessionID(ctx, workspaceSessionLookup{client}, sessionID)
		if err != nil {
			return err
		}
		sessionID = sess.ID
	}

	_, stopPprof := devtools.StartPprof()
	defer stopPprof()

	com := common.DefaultCommon(ctx, client, prefs)
	model := ui.NewRoot(com, sessionID, continueLast)

	inputFilter := ui.NewFilter()
	var env uv.Environ = os.Environ()
	program := tea.NewProgram(
		model,
		tea.WithEnvironment(env),
		tea.WithContext(ctx),
		tea.WithFilter(inputFilter.Filter),
	)
	model.SetSend(program.Send)
	pacedSend, stopPacing := ui.PaceMessages(func(msg any) { program.Send(msg) })
	go client.Subscribe(pacedSend)

	_, runErr := program.Run()
	stopPacing()
	model.Cleanup()

	printDaemonQuitNote(os.Stderr, client.AgentActivity())

	if runErr != nil {
		slog.Error("TUI run error", "error", runErr)
		return errors.New("Sennit crashed. Please copy the stacktrace above and open an issue at " + brand.RepoURL + "/issues/new?template=bug.yml") //nolint:staticcheck
	}
	return nil
}

// setupDaemonWorkspace finds or starts cwd's project daemon, connects a
// *grpcws.Client to it, and loads the client-side config that backs its
// UI preferences (CLIENT-SERVER.md, PR 0.5b/2.3) -- everything
// runInteractiveDaemon needs before it can hand a workspace to the TUI,
// factored out so a test can exercise the whole connect-and-configure
// path without driving an actual bubbletea program (mirrors
// setupLocalWorkspace/setupWorkspaceWithProgressBar's own split for the
// in-process path). supOpts.DataDir/Debug are overwritten from dataDir/
// debug; a test sets supOpts.Command to spawn its own daemon helper
// instead of a real sennit binary (see internal/daemon/supervisor's own
// test helper pattern).
//
// On success, cleanup shuts down the client's pump and closes its
// connection -- never the daemon itself, which keeps running any turn it
// was serving.
func setupDaemonWorkspace(ctx context.Context, cwd, dataDir string, debug bool, supOpts supervisor.Options) (client *grpcws.Client, prefs uiprefs.Store, cleanup func(), err error) {
	supOpts.DataDir = dataDir
	supOpts.Debug = debug

	socketPath, warning, err := ensureDaemonWithProgressBar(ctx, cwd, supOpts)
	if err != nil {
		var locked *supervisor.ErrTUILocked
		if errors.As(err, &locked) {
			// Same failure an in-process TUI would report for a lock
			// held by another sennit -- no daemon-specific wording to
			// add here, the message already names the owner.
			return nil, nil, nil, err
		}
		return nil, nil, nil, fmt.Errorf("%w\nRun with --no-daemon to use sennit without the daemon", err)
	}
	if warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}

	return connectDaemonWorkspace(ctx, cwd, dataDir, debug, socketPath)
}

// connectDaemonWorkspace is setupDaemonWorkspace's connect-only half: it
// dials socketPath, runs Connect (Hello + Snapshot), and loads the
// client-side config that backs UI preferences, without ever deciding
// whether to spawn a daemon -- a caller that has already found a socket
// itself (supervisor.ProbeRunning, never-spawn by design: `sennit
// attach`/`ps`) uses this directly instead of setupDaemonWorkspace, which
// would spawn one.
func connectDaemonWorkspace(ctx context.Context, cwd, dataDir string, debug bool, socketPath string) (client *grpcws.Client, prefs uiprefs.Store, cleanup func(), err error) {
	conn, err := supervisor.Dial(socketPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connecting to sennit daemon: %w\nRun with --no-daemon to use sennit without the daemon", err)
	}
	client = grpcws.NewClient(conn)

	connectCtx, cancel := context.WithTimeout(ctx, daemonConnectTimeout)
	err = client.Connect(connectCtx)
	cancel()
	if err != nil {
		client.Shutdown()
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("connecting to sennit daemon: %w\nRun with --no-daemon to use sennit without the daemon", err)
	}

	// UI preferences come from the client's own config, loaded from cwd
	// on this same machine -- the owner decision CLIENT-SERVER.md's
	// PR 0.5b records: at a remote daemon this would be a global-only
	// load instead, but that split is phase 3.
	//
	// config.LoadData, not configruntime.Load: the daemon we just
	// connected to already ran the real RuntimeProcessor pipeline
	// (provider discovery included) for this same project when it
	// booted, so repeating that here -- just to read options.tui.* --
	// would be a second, pointless round of discovery requests for
	// every single connect. LoadData reads the same merged config
	// (global+project layers, defaults applied) without a processor;
	// uiprefs only ever needs Options, never providers or credentials.
	cfgStore, err := config.LoadData(cwd, dataDir, debug)
	if err != nil {
		client.Shutdown()
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("loading local config for UI preferences: %w", err)
	}

	cleanup = func() {
		client.Shutdown()
		_ = conn.Close()
	}
	return client, uiPrefsStoreFromConfig(cfgStore), cleanup, nil
}

// ensureDaemonWithProgressBar wraps supervisor.EnsureRunning with the same
// indeterminate progress bar setupWorkspaceWithProgressBar shows for the
// in-process path, so a cold daemon start doesn't look like a hang either.
func ensureDaemonWithProgressBar(ctx context.Context, cwd string, opts supervisor.Options) (socketPath, warning string, err error) {
	showProgress := supportsProgressBar()
	if showProgress {
		_, _ = fmt.Fprintf(os.Stderr, ansi.SetIndeterminateProgressBar)
	}
	socketPath, warning, err = supervisor.EnsureRunning(ctx, cwd, opts)
	if showProgress {
		_, _ = fmt.Fprintf(os.Stderr, ansi.ResetProgressBar)
	}
	return socketPath, warning, err
}

// printDaemonQuitNote tells the person, once the TUI has exited in daemon
// mode, that quitting did not stop any turn the daemon is still running
// (CLIENT-SERVER.md, PR 2.3): the daemon keeps a busy session's turn
// going, and `sennit` alone reconnects to it. Prints nothing when
// activity shows no busy sessions. Takes the already-read
// workspace.AgentActivity rather than the client itself (a class-C
// getter, cache-only, no IO -- see CLIENT-SERVER.md's method table) so
// this is plain, testable logic with nothing to fake a connection for.
func printDaemonQuitNote(w io.Writer, activity workspace.AgentActivity) {
	if len(activity.BusySessions) == 0 {
		return
	}
	fmt.Fprintln(w, "\nWork continues in the background on the sennit daemon.")
	fmt.Fprintln(w, "Run `sennit` again to reconnect.")
}
