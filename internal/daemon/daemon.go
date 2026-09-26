// Package daemon runs a project's sennit backend headlessly, serving it
// to frontends over a unix socket instead of driving a bubbletea TUI
// (CLIENT-SERVER.md, PR 2.1). Run is the whole thing: bootstrap, listen,
// serve, and a graceful shutdown on context cancellation.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/app/threadspawn"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/daemon/sockpath"
	"github.com/rave-soft/sennit/internal/herdr"
	sennitlog "github.com/rave-soft/sennit/internal/log"
	"github.com/rave-soft/sennit/internal/projects"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/appws"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspacelock"
)

// staleDialTimeout bounds how long Run waits, when a socket file already
// exists, to find out whether anything is actually listening on it
// before concluding it is stale and safe to remove.
const staleDialTimeout = 200 * time.Millisecond

// defaultGracefulStopTimeout bounds how long Run waits for
// grpc.Server.GracefulStop to finish draining in-flight calls before it
// forces grpc.Server.Stop.
const defaultGracefulStopTimeout = 5 * time.Second

// ErrAlreadyRunning is returned by Run when another process is already
// listening on this project's socket.
var ErrAlreadyRunning = errors.New("daemon already running")

// Options configures Run. The first five fields mirror the flags
// setupLocalWorkspace (internal/cmd/root.go) reads for the interactive
// TUI, since Run bootstraps the same kind of App.
type Options struct {
	DataDir      string
	Debug        bool
	YOLO         bool
	Channels     []string
	TrustProject bool

	// GracefulStopTimeout overrides defaultGracefulStopTimeout; a test
	// shrinks it so a slow-draining fake call doesn't extend the test.
	GracefulStopTimeout time.Duration

	// Ready, if set, is called with the socket path once Run is
	// listening and about to start serving -- a caller (a test, in
	// particular) hook to dial without polling for the socket to appear.
	Ready func(socketPath string)

	// LogSetup, if set, replaces the default sennitlog.Setup call Run
	// makes once the data directory exists (PostConnect timing, same as
	// setupLocalWorkspace). A test that doesn't want to touch the
	// process-global logger (sennitlog.Setup's effect only applies once
	// per process anyway, see internal/log's initOnce) points this at a
	// no-op.
	LogSetup func(logFile string, debug bool)

	// IdlePollInterval overrides how often the idle monitor re-checks
	// busyness; defaults to defaultIdlePollInterval. A test shrinks this
	// so idle exit doesn't take real wall-clock minutes to observe --
	// options.daemon.idle_timeout still governs how long the daemon must
	// stay idle before it actually exits.
	IdlePollInterval time.Duration

	// AppReady, if set, is called with the bootstrapped *app.App once
	// Bootstrap succeeds, before Run starts serving. workspace.Workspace
	// (what a real frontend sees) has no way to raise a permission or
	// question request or drive an agent turn directly; this is a test
	// hook to reach the App underneath one for exactly that.
	AppReady func(a *app.App)

	// DelayBeforeListen, if set, pauses Run after Bootstrap succeeds
	// (lock held, in ModeDaemon with no socket recorded yet -- see
	// BootstrapOptions.WorkspaceLockMode) and before it binds the unix
	// socket. A test uses this to simulate a slow Bootstrap (cold MCP/
	// LSP init, a loaded machine) well past internal/daemon/supervisor's
	// own probing cadence, and assert EnsureRunning waits it out
	// (ModeDaemon + empty socket reads as "starting," never as a TUI)
	// instead of misreporting ErrTUILocked.
	DelayBeforeListen time.Duration
}

// ResolveSocketPath computes the socket path and workspace-lock
// directory a `daemon run` for cwd will use, without starting anything:
// the same config load, the same app.WorkspaceLockDir key, and the same
// sockpath.Path derivation Run itself does. internal/daemon/supervisor
// calls this rather than recomputing any of it, so a client can always
// find (or contend for) exactly the socket a real daemon would bind.
func ResolveSocketPath(ctx context.Context, cwd, dataDir string, debug bool) (socketPath, lockDir string, err error) {
	// config.LoadData merges every layer but skips the runtime processor,
	// so resolving a path does no provider discovery or credential
	// resolution: the supervisor calls this on every connect, and Run
	// calls it right before Bootstrap does the real load.
	cfg, err := config.LoadData(cwd, dataDir, debug)
	if err != nil {
		return "", "", fmt.Errorf("daemon: failed to load config: %w", err)
	}
	lockDir, err = app.WorkspaceLockDir(ctx, cfg.WorkingDir(), cfg.Config().Options.DataDirectory)
	if err != nil {
		return "", "", fmt.Errorf("daemon: failed to resolve workspace lock directory: %w", err)
	}
	socketPath, err = sockpath.Path(lockDir)
	if err != nil {
		return "", "", fmt.Errorf("daemon: failed to resolve socket path: %w", err)
	}
	return socketPath, lockDir, nil
}

// Run bootstraps a full app.App for the project at cwd -- exactly like
// the interactive TUI's setupLocalWorkspace, down to the workspace lock
// and threadspawn.Attach -- wraps it in an appws.AppWorkspace, and
// serves it over the project's unix socket (internal/daemon/sockpath)
// until ctx is canceled. It blocks until shutdown is complete: the
// socket stops accepting, in-flight RPCs drain (or are forced closed
// after GracefulStopTimeout), the App itself shuts down, the socket
// file is removed, and the workspace lock is released.
//
// Order matters here and must not be reshuffled: Bootstrap (and the
// workspace lock it acquires) runs BEFORE Run ever looks at the socket
// path. Only the lock's holder may touch that path -- checking for a
// live listener, removing a stale file, binding -- because a lock miss
// returns immediately, before any of that happens. The socket's actual
// mode/path is recorded into the lock file only once bind has
// succeeded (Lock.SetMode), not when the lock is first acquired: a
// reader must never see a socket path advertised that isn't live yet.
//
// The tempting alternative -- bind the socket first, then acquire the
// lock -- has a real failure mode: two processes starting at the same
// moment can both see no listener, both remove the (other's) socket
// file and bind their own, and then only one of them wins the lock.
// The lock loser exits and removes ITS socket path -- which by then may
// be the lock winner's own live listener, unlinked out from under it.
// The winner is left running or unreachable, and every later start
// finds the lock held with nothing answering on any socket. Acquiring
// the lock first makes that interleaving impossible: whichever process
// loses the lock returns before ever calling listen.
//
// If a socket file already exists and something answers a connection on
// it once this process holds the lock, Run returns ErrAlreadyRunning
// without touching that file -- see the ordering note above for why
// that should be unreachable in practice (the lock excludes any other
// live sennit for this project) and is kept as a defensive check
// anyway. A socket file with nothing listening (a previous daemon that
// crashed without cleaning up; flock itself is always released on
// process exit, but the socket special file is not) is stale and
// removed before Run listens in its place.
//
// Run disables the herdr client (CLIENT-SERVER.md, PR 1.5): herdr
// reports terminal-pane state for whichever process's pane it attaches
// to, and a daemon has no pane of its own -- attaching here would steal
// authority from whichever frontend's terminal actually owns it.
func Run(ctx context.Context, cwd string, opts Options) error {
	socketPath, _, err := ResolveSocketPath(ctx, cwd, opts.DataDir, opts.Debug)
	if err != nil {
		return err
	}

	logSetup := opts.LogSetup
	if logSetup == nil {
		logSetup = func(logFile string, debug bool) { sennitlog.Setup(logFile, debug) }
	}
	var logPath string
	boot, err := app.Bootstrap(ctx, cwd, app.BootstrapOptions{
		DataDir:       opts.DataDir,
		Debug:         opts.Debug,
		YOLO:          opts.YOLO,
		Channels:      opts.Channels,
		TrustProject:  opts.TrustProject,
		WorkspaceLock: true,
		// Acquire the lock already in ModeDaemon (empty socket, i.e.
		// "starting"), not the default ModeTUI: a reader (in particular
		// internal/daemon/supervisor) must be able to tell "a daemon is
		// still booting" from "a TUI holds this" for the whole span of
		// Bootstrap, not just from whenever SetMode runs below. See
		// BootstrapOptions.WorkspaceLockMode's doc comment.
		WorkspaceLockMode: workspacelock.ModeDaemon,
		HerdrClient:       func() *herdr.Client { return nil },
		// MCP OAuth must never open a browser on the daemon's own
		// machine (CLIENT-SERVER.md, PR 2.1): a client reaches the
		// authorization URL through MCPPendingAuth/MCPAuthURL instead.
		SuppressMCPBrowserAuth: true,
		PostDataDir: func(cfg *config.ConfigStore) error {
			if err := projects.Register(cwd, cfg.Config().Options.DataDirectory); err != nil {
				slog.Warn("Failed to register project", "error", err)
			}
			return nil
		},
		PostConnect: func(cfg *config.ConfigStore) error {
			logPath = config.GlobalLogFile()
			logSetup(logPath, opts.Debug)
			return nil
		},
		OnAppInitFailure: func(err error) {
			slog.Error("Failed to create daemon app instance", "error", err)
		},
	})
	if err != nil {
		// Bootstrap failed before or while acquiring the lock (most
		// commonly workspacelock.ErrLocked): this process never held
		// exclusivity over lockDir, so it must not touch socketPath --
		// it could belong to whoever does hold the lock.
		return err
	}
	if logPath != "" {
		fmt.Fprintln(os.Stderr, "Daemon log:", logPath)
	}

	if opts.DelayBeforeListen > 0 {
		select {
		case <-time.After(opts.DelayBeforeListen):
		case <-ctx.Done():
			boot.App.Shutdown()
			return ctx.Err()
		}
	}

	// From here on this process holds the workspace lock (boot.Lock),
	// which is what makes it safe to inspect and bind socketPath.
	lis, err := listen(ctx, socketPath)
	if err != nil {
		boot.App.Shutdown() // releases the lock
		return err
	}
	removeSocketOnExit := true
	defer func() {
		if removeSocketOnExit {
			_ = os.Remove(socketPath)
		}
	}()

	if opts.AppReady != nil {
		opts.AppReady(boot.App)
	}

	if err := boot.Lock.SetMode(workspacelock.ModeDaemon, socketPath); err != nil {
		// Non-fatal, matching Acquire's own treatment of a failed owner-
		// info write: the OS lock is what actually enforces exclusivity,
		// and a stale/missing socket annotation only degrades a future
		// reader's diagnostic, not correctness.
		slog.Debug("Failed to record daemon socket in workspace lock", "error", err)
	}

	threadspawn.Attach(ctx, boot.App, cwd, threadspawn.NewLocalSpawnerWithProjectPath(
		func() map[string]config.Agent { return boot.App.Config().UserAgents() },
		func() []*skills.Skill { return skills.Inheritable(boot.App.Skills.AllSkills()) },
		boot.App.PermissionsSkipFunc(),
		func() config.SelectedModel { return boot.App.Config().Model },
		boot.App.ProjectPath,
		func(a *app.App) workspace.Workspace { return appws.NewAppWorkspace(a, a.Store()) },
	))

	ws := appws.NewAppWorkspace(boot.App, boot.Config)

	// runCtx is what the shutdown select below actually waits on: ctx
	// cancels it from the caller's side (SIGTERM/SIGINT), the idle
	// monitor cancels it from ours once nothing has been busy for
	// idle_timeout (CLIENT-SERVER.md, PR 2.1), and a supervisor's
	// Shutdown RPC (PR 2.2) cancels it a third way once this daemon has
	// agreed to stand down. All three converge on the same graceful-
	// shutdown code beneath the select; the monitor itself is torn down
	// by this same cancellation, not separately.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	// grpcServer is referenced by shutdownHandler below before it is
	// assigned; that is fine because grpc.MethodDesc handlers -- and so
	// shutdownHandler itself -- only ever run once Serve has started,
	// which happens after the assignment a few lines down.
	var grpcServer *grpcws.Server
	shutdownHandler := func(ctx context.Context, onlyIfIdle bool) bool {
		if onlyIfIdle {
			// Exclude the calling client's own connection: begin() runs
			// before this handler even starts (see unaryInterceptor), so
			// without this the call asking "is anyone using this
			// daemon" would always see itself and answer busy.
			caller := grpcws.ClientIDFromContext(ctx)
			clients := &excludingClientCounter{server: grpcServer, exclude: caller}
			if busy, reason := (&idleBusyCheck{ws: ws, clients: clients}).busy(ctx); busy {
				slog.Info("Refusing conditional shutdown request: daemon is busy", "reason", reason)
				return false
			}
		}
		slog.Info("Shutting down on supervisor request", "only_if_idle", onlyIfIdle)
		runCancel()
		return true
	}
	var stopHub func()
	grpcServer, stopHub = grpcws.NewServer(ws, grpcws.WithShutdownHandler(shutdownHandler))

	if err := chmodSocket(socketPath); err != nil {
		slog.Warn("Failed to restrict daemon socket permissions", "path", socketPath, "error", err)
	}

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- grpcServer.Serve(lis) }()

	if opts.Ready != nil {
		opts.Ready(socketPath)
	}

	go runIdleMonitor(runCtx,
		&idleBusyCheck{ws: ws, clients: grpcServer},
		boot.Config.Config().Options.Daemon.EffectiveIdleTimeout(),
		opts.IdlePollInterval,
		runCancel,
	)

	select {
	case <-runCtx.Done():
	case err := <-serveErrCh:
		if err != nil {
			slog.Error("Daemon listener stopped unexpectedly", "error", err)
		}
	}

	stopTimeout := opts.GracefulStopTimeout
	if stopTimeout <= 0 {
		stopTimeout = defaultGracefulStopTimeout
	}
	stopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(stopTimeout):
		grpcServer.Stop()
		<-stopped
	}

	stopHub()
	boot.App.Shutdown()
	removeSocketOnExit = false
	_ = os.Remove(socketPath)

	return nil
}

// listen returns a listener on socketPath, first checking for -- and
// clearing -- a stale socket file left behind by a daemon that exited
// without cleaning up. It refuses to touch a socket something is
// actually answering on: that check must happen before any removal, or
// a live daemon's socket could be pulled out from under it by a second
// process racing to start.
func listen(ctx context.Context, socketPath string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return nil, fmt.Errorf("daemon: create socket directory: %w", err)
	}

	switch _, err := os.Stat(socketPath); {
	case err == nil:
		if socketIsLive(ctx, socketPath) {
			return nil, fmt.Errorf("%w: a process is already listening on %s", ErrAlreadyRunning, socketPath)
		}
		if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("daemon: remove stale socket %q: %w", socketPath, err)
		}
	case os.IsNotExist(err):
		// Nothing there yet; ListenConfig.Listen creates it.
	default:
		return nil, fmt.Errorf("daemon: stat socket %q: %w", socketPath, err)
	}

	var lc net.ListenConfig
	lis, err := lc.Listen(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("daemon: listen on %q: %w", socketPath, err)
	}
	return lis, nil
}

// socketIsLive reports whether a connection to path succeeds, i.e.
// something is actually listening there right now.
func socketIsLive(ctx context.Context, path string) bool {
	dialCtx, cancel := context.WithTimeout(ctx, staleDialTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "unix", path)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// chmodSocket restricts the socket to its owner. Windows has no POSIX
// permission bits to set here (AF_UNIX files there inherit the ACL of
// the directory they're created in), so this is a no-op on that
// platform rather than a call that would fail or silently do nothing
// useful.
func chmodSocket(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	return os.Chmod(path, 0o600)
}
