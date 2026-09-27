// Package supervisor auto-starts and version-checks a project's sennit
// daemon on behalf of a client (CLIENT-SERVER.md, PR 2.2). It is the
// client-side counterpart to internal/daemon: EnsureRunning finds an
// already-running daemon, single-flights a fresh one into existence
// when none answers, and reconciles a version mismatch between the
// daemon it found and this binary.
//
// This package must not import cobra: it is invoked by internal/cmd but
// has no notion of flags or commands of its own (mirrors the pre-C1
// supervisor's own doc comment, 027d6155c^:internal/server/supervisor/
// supervisor.go).
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/daemon"
	"github.com/rave-soft/sennit/internal/env"
	"github.com/rave-soft/sennit/internal/lock"
	"github.com/rave-soft/sennit/internal/version"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspacelock"
)

// defaultReadyTimeout bounds how long EnsureRunning waits for a freshly
// spawned daemon to answer a health check, when Options.ReadyTimeout is
// unset.
const defaultReadyTimeout = 10 * time.Second

// defaultProbeTimeout bounds a single health-check attempt, when
// Options.ProbeTimeout is unset. Short, like daemon.Run's own
// staleDialTimeout: a probe that hangs must not itself eat the ready
// budget.
const defaultProbeTimeout = 200 * time.Millisecond

// pollInterval is how often waitForReady retries the health check while
// waiting for a freshly spawned daemon to come up.
const pollInterval = 100 * time.Millisecond

// startLockName is the file single-flighting a spawn locks, inside the
// same directory workspacelock itself locks (lockDir -- see
// daemon.ResolveSocketPath). It is a second, independent flock: holding
// it says nothing about the workspace lock itself, only "I am the one
// deciding whether to spawn a daemon for this project right now."
const startLockName = "start.lock"

// startupLogName is where spawnDetached redirects a freshly spawned
// daemon's stdout/stderr, so a readiness timeout can quote its tail.
const startupLogName = "daemon-startup.log"

// Options configures EnsureRunning.
type Options struct {
	// DataDir and Debug are forwarded to a spawned daemon (`daemon run
	// --data-dir ... --debug`) exactly as given, and used locally to
	// resolve the same project config a `daemon run` for cwd would load
	// (see daemon.ResolveSocketPath).
	DataDir string
	Debug   bool

	// ReadyTimeout bounds the total wait for a freshly spawned (or
	// restarted) daemon to answer a health check. Defaults to
	// defaultReadyTimeout.
	ReadyTimeout time.Duration

	// ProbeTimeout bounds a single health-check attempt. Defaults to
	// defaultProbeTimeout.
	ProbeTimeout time.Duration

	// Command, if set, replaces os.Executable()+exec.Command as how a
	// daemon is spawned: it receives the arguments EnsureRunning would
	// otherwise pass to the real binary ("daemon", "run", "--cwd",
	// cwd, ...) and returns the *exec.Cmd to run. A test uses this to
	// spawn its own test binary (re-executed with a different
	// -test.run) as the daemon helper instead of a real `sennit`
	// binary. EnsureRunning still sets Stdin/Stdout/Stderr/Env and
	// detaches the process itself, regardless of where the *exec.Cmd
	// came from.
	Command func(args []string) *exec.Cmd
}

func (o Options) readyTimeout() time.Duration {
	if o.ReadyTimeout > 0 {
		return o.ReadyTimeout
	}
	return defaultReadyTimeout
}

func (o Options) probeTimeout() time.Duration {
	if o.ProbeTimeout > 0 {
		return o.ProbeTimeout
	}
	return defaultProbeTimeout
}

// ErrTUILocked is returned by EnsureRunning when a project's workspace
// lock is held by an interactive TUI (or any other in-process, embedded
// caller), not a daemon -- the same condition an embedded TUI reports to
// its own user as workspacelock.ErrLocked (Unwrap returns it), so a
// caller doesn't need a second, unrelated error string for "another
// sennit is using this project."
type ErrTUILocked struct {
	PID   int
	cause error
}

func (e *ErrTUILocked) Error() string { return e.cause.Error() }
func (e *ErrTUILocked) Unwrap() error { return e.cause }

// ErrProtocolMismatch is returned by EnsureRunning when the daemon it
// found speaks a different wire ProtocolVersion than this client
// expects, and the daemon is busy (so EnsureRunning would not restart it
// even though it asked).
type ErrProtocolMismatch struct {
	ServerProtocolVersion int
	ClientProtocolVersion int
}

func (e *ErrProtocolMismatch) Error() string {
	return fmt.Sprintf(
		"the running daemon speaks protocol version %d but this client expects %d, and the daemon is busy so it could not be restarted; run `sennit daemon restart` once it is idle",
		e.ServerProtocolVersion, e.ClientProtocolVersion,
	)
}

// errRestartNeeded is an internal sentinel: checkVersion returns it to
// tell EnsureRunning "the daemon agreed to shut down over a version
// mismatch; spawn a fresh one," rather than surfacing it to a caller.
var errRestartNeeded = errors.New("supervisor: daemon is restarting over a version mismatch")

// EnsureRunning finds (or starts) the daemon serving cwd's project and
// returns the unix socket it can be reached on. It never blocks longer
// than necessary: an already-healthy daemon is returned immediately
// (Options.ProbeTimeout budget); starting a fresh one is single-flighted
// across every concurrent caller for this project via startLockName, so
// eight processes calling this at once still end up with exactly one
// daemon.
//
// warning is non-empty exactly when EnsureRunning is handing back a
// daemon whose BuildID differs from this client's own and which could
// not be restarted because it is busy -- the caller should print it, not
// treat it as failure.
func EnsureRunning(ctx context.Context, cwd string, opts Options) (socketPath, warning string, err error) {
	socketPath, lockDir, err := daemon.ResolveSocketPath(ctx, cwd, opts.DataDir, opts.Debug)
	if err != nil {
		return "", "", err
	}

	sp, running, err := probeRunning(ctx, lockDir, opts)
	if err != nil {
		return "", "", err
	}
	if !running {
		sp, err = spawnSingleFlight(ctx, cwd, lockDir, socketPath, opts)
		if err != nil {
			return "", "", err
		}
	}

	warning, err = checkVersion(ctx, sp)
	if errors.Is(err, errRestartNeeded) {
		if err := awaitSocketGone(ctx, sp); err != nil {
			return "", "", err
		}
		sp, err = spawnSingleFlight(ctx, cwd, lockDir, socketPath, opts)
		if err != nil {
			return "", "", err
		}
		return sp, "", nil
	}
	if err != nil {
		return "", "", err
	}
	return sp, warning, nil
}

// probeRunning reports whether a healthy daemon already answers for this
// project. It reads workspacelock's own record and switches on Mode:
//
//   - No record at all: not running, nothing to disambiguate.
//   - ModeTUI: an embedded TUI (or any other in-process, non-daemon
//     caller) holds the workspace lock. daemon.Run acquires its lock
//     already in ModeDaemon (see BootstrapOptions.WorkspaceLockMode's
//     doc comment), not the package default ModeTUI, precisely so this
//     case is unambiguous here -- a daemon never reports as ModeTUI, at
//     any point in its life, including while still mid-Bootstrap. This
//     still re-verifies the OS flock itself (workspaceLockFree) before
//     refusing: the record survives a crash (kill -9 releases the flock
//     but leaves the file's last-written contents in place), so a
//     stale ModeTUI record must not be trusted on its own.
//   - ModeDaemon with an empty Socket: a daemon has acquired the lock
//     but hasn't bound its socket yet ("starting" -- see daemon.Run's
//     own doc comment on why Lock.SetMode only runs once bind
//     succeeds). Not running yet, but not an error either; the caller
//     waits for it.
//   - ModeDaemon with a Socket: health-check it.
func probeRunning(ctx context.Context, lockDir string, opts Options) (sp string, running bool, err error) {
	info, ok, err := workspacelock.CurrentOwner(lockDir)
	if err != nil {
		return "", false, fmt.Errorf("supervisor: reading workspace lock: %w", err)
	}
	if !ok {
		return "", false, nil
	}
	if info.Mode == workspacelock.ModeTUI {
		return refuseIfLocked(lockDir, info)
	}
	if info.Socket == "" {
		return "", false, nil
	}
	if probeHealthy(ctx, info.Socket, opts.probeTimeout()) {
		return info.Socket, true, nil
	}
	return "", false, nil
}

// refuseIfLocked re-verifies a ModeTUI record's OS flock is genuinely
// still held before returning ErrTUILocked, distinguishing a live
// holder from one whose crash left the record behind but released the
// flock (see probeRunning's doc comment). It is safe to call
// unsynchronized, from any number of concurrent EnsureRunning callers
// at once, unlike a probe built on workspacelock.Acquire would be --
// see workspaceLockFree's own doc comment.
func refuseIfLocked(lockDir string, info workspacelock.OwnerInfo) (string, bool, error) {
	free, err := workspaceLockFree(lockDir)
	if err != nil {
		return "", false, fmt.Errorf("supervisor: probing workspace lock: %w", err)
	}
	if free {
		return "", false, nil
	}
	return "", false, &ErrTUILocked{PID: info.PID, cause: fmt.Errorf(
		"%w: %s (owner pid=%d mode=%s)", workspacelock.ErrLocked, lockDir, info.PID, info.Mode,
	)}
}

// workspaceLockFree reports whether lockDir's workspace lock is free
// right now, without ever writing to the lock file: unlike
// workspacelock.Acquire, which records this process's own owner info as
// a side effect of succeeding (a side effect that would be actively
// misleading here -- every concurrent EnsureRunning caller for this
// project probes the same lockDir before any of them holds startLockName,
// so a naive Acquire-then-Release probe could stamp the file with an
// arbitrary caller's PID under ModeTUI in between two real owners, and a
// sibling caller reading it a moment later would misdiagnose that as a
// second TUI). internal/lock.TryFile -- the same primitive
// workspacelock.Acquire itself uses underneath, on the exact same path,
// so flock's inode-keyed semantics stay consistent with it -- takes and
// immediately releases the raw OS lock with no JSON write at all, which
// is also what makes it safe to call from probeRunning unsynchronized
// (no start.lock held), not just from spawnSingleFlight.
func workspaceLockFree(lockDir string) (bool, error) {
	release, err := lock.TryFile(filepath.Join(lockDir, brand.LockFile))
	if err == nil {
		release()
		return true, nil
	}
	if errors.Is(err, lock.ErrContended) {
		return false, nil
	}
	return false, err
}

// spawnSingleFlight takes startLockName, so at most one caller across
// every process on the machine spawns a daemon for this project at a
// time, re-checks whether a concurrent caller already finished (or the
// workspace lock is now held by something else), and only then spawns.
//
// It loops, bounded by opts.readyTimeout(), rather than deciding once:
// the workspace lock can be held by a daemon that is neither ready to
// answer a health check yet (still mid-Bootstrap, socket not bound --
// wait, it will become healthy) nor going to spawn a replacement for us
// (mid-shutdown, e.g. one this same caller just asked to restart over a
// version mismatch -- see checkVersion/errRestartNeeded in EnsureRunning:
// its socket can stop answering health checks before its process
// actually releases the workspace lock, since the two are sequential
// steps of the same shutdown). Only re-probing periodically, rather
// than committing to one branch up front, covers both without needing
// to tell them apart.
func spawnSingleFlight(ctx context.Context, cwd, lockDir, socketPath string, opts Options) (string, error) {
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return "", fmt.Errorf("supervisor: create lock directory %q: %w", lockDir, err)
	}
	release, err := lock.File(ctx, filepath.Join(lockDir, startLockName))
	if err != nil {
		return "", fmt.Errorf("supervisor: acquire start lock: %w", err)
	}
	defer release()

	deadline := time.Now().Add(opts.readyTimeout())
	for {
		if sp, running, err := probeRunning(ctx, lockDir, opts); err != nil {
			return "", err
		} else if running {
			return sp, nil
		}

		free, err := workspaceLockFree(lockDir)
		if err != nil {
			return "", fmt.Errorf("supervisor: probing workspace lock: %w", err)
		}
		if free {
			// Nobody holds the workspace lock: any socket file left at
			// socketPath is stale, from a daemon that crashed (or is
			// mid-shutdown) without cleaning up (daemon.Run replaces
			// it too; this is belt-and-suspenders so a health probe
			// against it never gets confused).
			_ = os.Remove(socketPath)

			logPath, daemonLogPath, err := spawnDetached(cwd, lockDir, opts)
			if err != nil {
				return "", err
			}
			return waitForReady(ctx, socketPath, logPath, daemonLogPath, opts)
		}

		if time.Now().After(deadline) {
			return "", errors.New("supervisor: timed out waiting for the workspace lock to become available or the holder to answer a health check")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// spawnDetached starts a `daemon run --cwd cwd [--data-dir ...] [--debug]`
// process that outlives this one: stdin is /dev/null, stdout/stderr go
// to a per-project startup log, the environment carries no HERDR_*
// variables (see internal/env.WithoutHerdrEnv's doc comment -- otherwise
// the daemon would permanently attach to whichever terminal pane
// happened to spawn it first), and the process is put in its own
// session/process group (detachProcess) so it is not sent our signals
// and does not die when we exit. It returns the startup log's path, the
// path the child's own logger will write to once it gets that far
// (config.LogFileForPID, computed from the child's PID -- the startup
// log only ever holds what ran before that point), and releases the
// child (os.Process.Release) without waiting on it beyond confirming
// Start succeeded.
func spawnDetached(cwd, lockDir string, opts Options) (logPath, daemonLogPath string, err error) {
	args := []string{"daemon", "run", "--cwd", cwd}
	if opts.DataDir != "" {
		args = append(args, "--data-dir", opts.DataDir)
	}
	if opts.Debug {
		args = append(args, "--debug")
	}

	var cmd *exec.Cmd
	if opts.Command != nil {
		cmd = opts.Command(args)
	} else {
		exe, err := os.Executable()
		if err != nil {
			return "", "", fmt.Errorf("supervisor: resolve executable: %w", err)
		}
		// context.Background(), not ctx: the whole point of this
		// process is to outlive the caller (see this func's own doc
		// comment) -- exec.CommandContext is used here only to satisfy
		// the noctx lint rule, its cancellation is never wired to
		// anything.
		cmd = exec.CommandContext(context.Background(), exe, args...) //nolint:gosec // exe is this same binary's own path.
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return "", "", fmt.Errorf("supervisor: open %s: %w", os.DevNull, err)
	}
	defer devNull.Close()
	cmd.Stdin = devNull

	logPath = filepath.Join(lockDir, startupLogName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", "", fmt.Errorf("supervisor: open startup log %q: %w", logPath, err)
	}
	defer logFile.Close()
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	cmd.Env = env.WithoutHerdrEnv(os.Environ())
	detachProcess(cmd)

	if err := cmd.Start(); err != nil {
		return "", "", fmt.Errorf("supervisor: start daemon: %w", err)
	}
	daemonLogPath = config.LogFileForPID(cmd.Process.Pid)
	if err := cmd.Process.Release(); err != nil {
		return "", "", fmt.Errorf("supervisor: release daemon process: %w", err)
	}
	return logPath, daemonLogPath, nil
}

// waitForReady polls socketPath's health check every pollInterval until
// it answers SERVING or opts.readyTimeout elapses, in which case the
// returned error quotes both logPath's tail and daemonLogPath's tail (if
// any) so the caller sees why the daemon it just spawned never came up
// -- a hang past "Initializing MCP clients" (the last line the startup
// log sees before Bootstrap hands off to the real logger) is otherwise
// invisible, since everything after that point goes only to
// daemonLogPath.
func waitForReady(ctx context.Context, socketPath, logPath, daemonLogPath string, opts Options) (string, error) {
	deadline := time.Now().Add(opts.readyTimeout())
	for {
		if probeHealthy(ctx, socketPath, opts.probeTimeout()) {
			return socketPath, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("supervisor: timed out waiting for daemon to become ready%s%s",
				logTail("daemon startup log", logPath), logTail("daemon log", daemonLogPath))
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// awaitSocketGone waits for a daemon that has committed to shutting
// down (a Shutdown RPC it just accepted) to actually stop answering,
// bounded by ctx, before a caller spawns its replacement -- otherwise
// the replacement could bind while the outgoing daemon is still mid-
// GracefulStop and briefly serving stale state.
func awaitSocketGone(ctx context.Context, socketPath string) error {
	for {
		if !probeHealthy(ctx, socketPath, defaultProbeTimeout) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// logTail returns the last portion of the file at path under label,
// formatted for appending to an error message, or "" if path is empty
// or unreadable -- unreadable includes "doesn't exist yet," which is
// the ordinary case for daemonLogPath when the child never got far
// enough to open its own logger.
func logTail(label, path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	const maxTail = 4000
	if len(data) > maxTail {
		data = data[len(data)-maxTail:]
	}
	return fmt.Sprintf("\n--- %s (%s) ---\n%s", label, path, data)
}

// ProbeRunning reports whether a daemon is already running and healthy
// for cwd's project, without starting one -- the "never spawn" half of
// EnsureRunning, used by `sennit attach`/`ps`/`daemon status` and a
// plain `sennit run` (CLIENT-SERVER.md, PR 2.3), none of which may spawn
// a daemon just to answer their own question. err is
// *ErrTUILocked when the project's workspace lock is held by an
// embedded TUI rather than a daemon, exactly as EnsureRunning reports
// it.
func ProbeRunning(ctx context.Context, cwd string, opts Options) (socketPath string, running bool, err error) {
	_, lockDir, err := daemon.ResolveSocketPath(ctx, cwd, opts.DataDir, opts.Debug)
	if err != nil {
		return "", false, err
	}
	return probeRunning(ctx, lockDir, opts)
}

// ProbeHealthy is probeHealthy's exported form, for a caller (`sennit
// daemon status`/`stop`) that already has a socket path in hand (e.g.
// from workspacelock.CurrentOwner) and needs a liveness check without
// going through EnsureRunning's find-or-spawn logic.
func ProbeHealthy(ctx context.Context, socketPath string, timeout time.Duration) bool {
	return probeHealthy(ctx, socketPath, timeout)
}

// AwaitGone waits, bounded by ctx, for socketPath to stop answering
// health checks -- used after a Shutdown RPC is accepted, by
// EnsureRunning's own version-mismatch restart and by `sennit daemon
// stop`/`restart`, which must not report success (or spawn a
// replacement) while the outgoing daemon is still mid-GracefulStop.
func AwaitGone(ctx context.Context, socketPath string) error {
	return awaitSocketGone(ctx, socketPath)
}

// StartupLogPath returns the path a spawned daemon's stdout/stderr is
// redirected to for lockDir -- the same path spawnDetached writes to.
// `sennit daemon logs` shows it alongside the daemon's own process log so
// a cold start that never got far enough to open its real logger is
// still visible somewhere.
func StartupLogPath(lockDir string) string {
	return filepath.Join(lockDir, startupLogName)
}

// Dial opens a lazy (unconnected until first RPC) *grpc.ClientConn to the
// unix socket EnsureRunning returned, using the same dial options this
// package's own version/health probes use (grpcws.DefaultClientDialOptions).
// A caller building a real frontend (internal/cmd's daemon-mode root
// command) uses this rather than duplicating the dialer, so the client and
// this package's own bookkeeping calls always agree on how the socket is
// reached.
func Dial(socketPath string) (*grpc.ClientConn, error) {
	return dialConn(socketPath)
}

// dialConn is Dial's unexported body; see Dial's doc comment.
func dialConn(socketPath string) (*grpc.ClientConn, error) {
	opts := append(grpcws.DefaultClientDialOptions(),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	return grpc.NewClient("passthrough:///"+socketPath, opts...)
}

// probeHealthy reports whether socketPath answers the standard gRPC
// health check as SERVING within timeout. Any failure (nothing
// listening, a stale/orphaned file, a slow or wedged daemon) reports
// false rather than erroring -- every caller here already treats "not
// healthy yet" and "definitely not running" as the same "keep going"
// signal.
func probeHealthy(ctx context.Context, socketPath string, timeout time.Duration) bool {
	conn, err := dialConn(socketPath)
	if err != nil {
		return false
	}
	defer conn.Close()

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := grpc_health_v1.NewHealthClient(conn).Check(probeCtx, &grpc_health_v1.HealthCheckRequest{})
	return err == nil && resp.GetStatus() == grpc_health_v1.HealthCheckResponse_SERVING
}

// versionAction is checkVersion's classification of a Hello response
// against this build's own versions -- split out as classifyVersion so
// it can be unit-tested without a real server (see supervisor_test.go).
type versionAction int

const (
	versionOK versionAction = iota
	versionProtocolMismatch
	versionBuildMismatch
)

// classifyVersion compares hello against this client's own
// grpcws.ProtocolVersion and version.Commit (what HelloResponse.BuildID
// calls itself on the reporting side -- see metaServer.Hello).
func classifyVersion(hello grpcws.HelloResponse) versionAction {
	if hello.ProtocolVersion != grpcws.ProtocolVersion {
		return versionProtocolMismatch
	}
	if hello.BuildID != version.Commit {
		return versionBuildMismatch
	}
	return versionOK
}

// checkVersion connects to socketPath, calls Hello, and reconciles any
// version skew against this build (CLIENT-SERVER.md, PR 2.2): a
// ProtocolVersion mismatch or a differing BuildID both ask the daemon to
// shut down only if it is idle (Meta.Shutdown{OnlyIfIdle: true}) --
// never forcing out a busy one. A ProtocolVersion mismatch that could
// not be resolved this way is an error (the two sides may not even
// agree on the wire format); a BuildID-only mismatch that couldn't be
// resolved is a warning: the daemon is still usable, just not this
// exact build.
//
// err is errRestartNeeded (via errors.Is) exactly when the daemon
// accepted the shutdown request; the caller must wait for its socket to
// stop answering and spawn a fresh one.
func checkVersion(ctx context.Context, socketPath string) (warning string, err error) {
	conn, err := dialConn(socketPath)
	if err != nil {
		return "", fmt.Errorf("supervisor: dial daemon: %w", err)
	}
	defer conn.Close()
	client := grpcws.NewClient(conn)
	defer client.Shutdown()

	hello, err := client.Hello(ctx)
	if err != nil {
		return "", fmt.Errorf("supervisor: hello: %w", err)
	}

	switch classifyVersion(hello) {
	case versionOK:
		return "", nil
	case versionProtocolMismatch:
		accepted, rerr := client.RequestShutdown(ctx, true)
		if rerr != nil {
			return "", fmt.Errorf("supervisor: requesting shutdown of a protocol-mismatched daemon: %w", rerr)
		}
		if !accepted {
			return "", &ErrProtocolMismatch{
				ServerProtocolVersion: hello.ProtocolVersion,
				ClientProtocolVersion: grpcws.ProtocolVersion,
			}
		}
		return "", errRestartNeeded
	default: // versionBuildMismatch
		accepted, rerr := client.RequestShutdown(ctx, true)
		if rerr != nil {
			return "", fmt.Errorf("supervisor: requesting shutdown of a stale-build daemon: %w", rerr)
		}
		if accepted {
			return "", errRestartNeeded
		}
		return fmt.Sprintf(
			"using a running daemon built as %q (this client is built as %q); it is busy, so it was not restarted -- run `sennit daemon restart` once it is idle",
			hello.BuildID, version.Commit,
		), nil
	}
}
