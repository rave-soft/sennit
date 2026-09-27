package daemon_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/testenv"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/daemon"
	"github.com/rave-soft/sennit/internal/daemon/sockpath"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspacelock"
)

// mockGlobalConfig follows AGENTS.md's "Testing without real providers"
// recipe: a provider that only exists for the test, with the embedded
// catalog disabled, so nothing here can reach a real endpoint.
const mockGlobalConfig = `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`

// testTimeout bounds every daemon.Run call this file makes, directly or
// through startDaemon/the subprocess helpers below: a stuck Run (a
// deadlock in the ordering it documents, a graceful-stop that never
// drains) must fail a test loudly within this budget rather than hang
// the suite. Passed to context.WithTimeout, never context.WithCancel
// alone, for every ctx a daemon.Run call in this file receives, and used
// as exec.CommandContext's ctx for every subprocess helper so a stuck
// child is killed rather than left running past the test.
//
// Widened under -race (raceIdleScale, idle_test.go): these tests spawn
// real subprocesses and dial real unix sockets, and -race's own overhead
// plus CI's cross-package CPU contention (AGENTS.md's "wall-clock budgets
// under -race") can make that comfortably slower than in isolation. This
// is a hang guard, not a performance assertion, so widening it costs
// nothing but wall time on a genuine hang.
var testTimeout = raceIdleScale(20 * time.Second)

// shutdownWait bounds awaitShutdown's own select, independent of the
// ctx a test just canceled to *trigger* that shutdown: waiting on that
// same ctx's Done() would race against the cancellation that starts the
// wait (Done() is already closed by the time awaitShutdown runs), so
// this is a fresh timer instead.
var shutdownWait = raceIdleScale(10 * time.Second)

// writeGlobalConfig points the global config location at a fresh
// directory for this test and seeds it with mockGlobalConfig. Mirrors
// internal/agent/common_test.go's helper of the same name; not shared
// between the packages to avoid a test-only cross-package dependency.
func writeGlobalConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", filepath.Join(dir, "data"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sennit.json"), []byte(mockGlobalConfig), 0o644))
}

// projectSocketPath computes the same socket path daemon.Run will,
// independently, so a test can dial it or plant a stale/sentinel file at
// it before the daemon ever starts.
func projectSocketPath(t *testing.T, projectDir string) string {
	t.Helper()
	cfg, err := configruntime.Load(projectDir, "", false)
	require.NoError(t, err)
	lockDir, err := app.WorkspaceLockDir(t.Context(), cfg.WorkingDir(), cfg.Config().Options.DataDirectory)
	require.NoError(t, err)
	p, err := sockpath.Path(lockDir)
	require.NoError(t, err)
	return p
}

// projectLockDir mirrors projectSocketPath but returns the lock
// directory itself, for a test that acquires the workspace lock
// directly (simulating a TUI holding it) rather than through Run.
func projectLockDir(t *testing.T, projectDir string) string {
	t.Helper()
	cfg, err := configruntime.Load(projectDir, "", false)
	require.NoError(t, err)
	lockDir, err := app.WorkspaceLockDir(t.Context(), cfg.WorkingDir(), cfg.Config().Options.DataDirectory)
	require.NoError(t, err)
	return lockDir
}

// dialDaemon connects a *grpcws.Client to socketPath over a real unix
// socket -- not bufconn -- using grpcws.DefaultClientDialOptions, exactly
// as a real frontend would. The returned close func tears the client
// down immediately; a test must call it before canceling the daemon's
// context, or the daemon's own graceful shutdown has to wait out this
// still-open connection's event stream until GracefulStopTimeout forces
// a hard Stop -- correct, but needlessly slow. t.Cleanup calls it again
// as a backstop (both Client.Shutdown and grpc.ClientConn.Close are
// idempotent), for a test that exits before reaching its own call.
func dialDaemon(t *testing.T, socketPath string) (client *grpcws.Client, closeClient func()) {
	t.Helper()
	dialOpts := append(grpcws.DefaultClientDialOptions(),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	conn, err := grpc.NewClient("passthrough:///"+socketPath, dialOpts...)
	require.NoError(t, err)

	client = grpcws.NewClient(conn)
	require.NoError(t, client.Connect(t.Context()))
	closeClient = func() {
		client.Shutdown()
		_ = conn.Close()
	}
	t.Cleanup(closeClient)
	return client, closeClient
}

// startDaemon runs daemon.Run for projectDir in a goroutine and blocks
// until it reports its socket ready (or exits early with an error). It
// returns the socket path, and channels the test uses to cancel it and
// observe Run's return. Callers must pass a ctx that carries its own
// deadline (context.WithTimeout, not a bare WithCancel) so a Run that
// never becomes ready and never returns still unblocks this call.
//
// This in-process form is fine for these lifecycle tests, which each
// start at most one daemon.Run per project directory. It is NOT used for
// the two-daemons-racing tests below: workspacelock.Acquire deliberately
// shares one OS lock across concurrent in-process acquirers for the same
// directory (poolEntry refcounting -- see workspacelock.go), which would
// make two goroutines here indistinguishable from two calls made by
// cooperating parts of one sennit process. Only a real second OS process
// reproduces the race those tests guard against.
func startDaemon(t *testing.T, ctx context.Context, projectDir string) (socketPath string, runErr <-chan error) {
	t.Helper()
	ready := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- daemon.Run(ctx, projectDir, daemon.Options{
			Ready:    func(s string) { ready <- s },
			LogSetup: func(string, bool) {}, // avoid touching the process-global file logger in tests
		})
	}()

	select {
	case socketPath = <-ready:
	case err := <-errCh:
		t.Fatalf("daemon exited before becoming ready: %v", err)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for daemon to become ready: %v", ctx.Err())
	}
	return socketPath, errCh
}

func awaitShutdown(t *testing.T, runErr <-chan error) {
	t.Helper()
	select {
	case err := <-runErr:
		require.NoError(t, err)
	case <-time.After(shutdownWait):
		t.Fatal("timed out waiting for daemon to shut down")
	}
}

// TestDaemon_HelloListSessionsSnapshot is the end-to-end smoke test: a
// real gRPC client dialing a real unix socket, exercising Hello (the
// hand-written meta RPC), a generated unary call, and Snapshot -- the
// three surfaces PR 2.1's acceptance criteria call out by name.
func TestDaemon_HelloListSessionsSnapshot(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	socketPath, runErr := startDaemon(t, ctx, projectDir)
	client, closeClient := dialDaemon(t, socketPath)

	hello, err := client.Hello(t.Context())
	require.NoError(t, err)
	require.NotZero(t, hello.ProtocolVersion)

	_, err = client.ListSessions(t.Context())
	require.NoError(t, err)

	_, err = client.Snapshot(t.Context())
	require.NoError(t, err)

	closeClient()
	cancel()
	awaitShutdown(t, runErr)

	_, statErr := os.Stat(socketPath)
	require.True(t, os.IsNotExist(statErr), "expected the socket file to be removed after shutdown")
}

// TestDaemon_SecondStartFailsThenSucceedsAfterFirstStops covers the
// single-daemon-per-project contract: a second `daemon run` against a
// project a first daemon is already serving must fail with
// ErrAlreadyRunning rather than fighting it for the socket; canceling
// the first must free everything (socket file and workspace lock) for a
// third start to succeed.
func TestDaemon_SecondStartFailsThenSucceedsAfterFirstStops(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	socketPath, firstErr := startDaemon(t, ctx, projectDir)

	secondCtx, secondCancel := context.WithTimeout(t.Context(), testTimeout)
	defer secondCancel()
	err := daemon.Run(secondCtx, projectDir, daemon.Options{LogSetup: func(string, bool) {}})
	require.Error(t, err)
	require.ErrorIs(t, err, daemon.ErrAlreadyRunning)

	// The first daemon's socket must be untouched by the failed second
	// attempt.
	client, closeClient := dialDaemon(t, socketPath)
	_, err = client.Hello(t.Context())
	require.NoError(t, err)

	closeClient()
	cancel()
	awaitShutdown(t, firstErr)

	thirdCtx, thirdCancel := context.WithTimeout(t.Context(), testTimeout)
	defer thirdCancel()
	_, thirdErr := startDaemon(t, thirdCtx, projectDir)
	thirdCancel()
	awaitShutdown(t, thirdErr)
}

// TestDaemon_StaleSocketFileIsReplaced covers a daemon that exited
// without cleaning up (a kill -9, in practice): its socket special file
// is gone by the time a real listener would answer it, but a regular
// file (or a leftover, no-longer-listened-on socket node) can still be
// sitting at that path. Run must detect that nothing answers there and
// replace it, not refuse to start.
func TestDaemon_StaleSocketFileIsReplaced(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)

	projectDir := t.TempDir()
	socketPath := projectSocketPath(t, projectDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(socketPath), 0o700))
	require.NoError(t, os.WriteFile(socketPath, []byte("stale"), 0o600))

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	gotSocketPath, runErr := startDaemon(t, ctx, projectDir)
	require.Equal(t, socketPath, gotSocketPath)

	client, closeClient := dialDaemon(t, socketPath)
	_, err := client.Hello(t.Context())
	require.NoError(t, err)

	closeClient()
	cancel()
	awaitShutdown(t, runErr)
}

// ---------------------------------------------------------------------
// Subprocess helpers for the two cross-process tests below.
//
// workspacelock.Acquire intentionally shares one OS lock across
// concurrent in-process acquirers for the same directory (see
// poolEntry's doc comment in workspacelock.go): two goroutines in this
// test binary calling Acquire for the same lockDir are, as far as that
// package is concerned, two callers within ONE sennit process
// legitimately sharing it -- not two competing daemons. Reproducing the
// actual cross-process race (and the actual cross-process exclusion the
// fix relies on) needs a second real OS process. internal/app's
// bootstrap_test.go already established this pattern
// (TestWorkspaceLockHelperProcess / requireWorkspaceLockContended) for
// exactly the same reason; these mirror it for this package, since
// os.Args[0] always re-executes the CURRENT package's test binary.
// ---------------------------------------------------------------------

// TestDaemonLockHelperProcess is a subprocess helper (gated by
// SENNIT_DAEMON_LOCK_HELPER=1) that acquires the workspace lock at the
// directory given as its last argument and holds it until stdin closes.
// It simulates a TUI (or another daemon) already holding the project's
// workspace lock in a genuinely separate OS process -- see
// TestDaemon_FailsWhenTUIHoldsLock.
func TestDaemonLockHelperProcess(t *testing.T) {
	if os.Getenv("SENNIT_DAEMON_LOCK_HELPER") != "1" {
		return
	}
	lockDir := os.Args[len(os.Args)-1]
	lock, err := workspacelock.Acquire(lockDir)
	if err != nil {
		fmt.Fprintln(os.Stdout, err)
		os.Exit(1)
	}
	defer lock.Release()
	fmt.Fprintln(os.Stdout, "locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// lockHelper is a running TestDaemonLockHelperProcess subprocess.
type lockHelper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

// startLockHelper starts a lock-holding subprocess for lockDir and
// blocks until it reports the lock acquired.
func startLockHelper(t *testing.T, lockDir string) *lockHelper {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestDaemonLockHelperProcess", "--", lockDir)
	cmd.Env = append(os.Environ(), "SENNIT_DAEMON_LOCK_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	h := &lockHelper{cmd: cmd, stdin: stdin}
	t.Cleanup(h.stop)

	line := readLineBounded(t, bufio.NewReader(stdout), testTimeout)
	require.Equal(t, "locked\n", line)
	return h
}

func (h *lockHelper) stop() {
	_ = h.stdin.Close()
	_ = h.cmd.Wait()
}

// TestDaemonHelperProcess is a subprocess helper (gated by
// SENNIT_DAEMON_HELPER=1) that runs daemon.Run for the project
// directory given as its last argument, in a genuinely separate OS
// process. It prints exactly one status line to stdout as soon as its
// outcome is known ("ready <socket>\n" or "error: <message>\n"); once
// ready, it keeps serving until stdin closes, then shuts down and prints
// "stopped\n" before exiting. See TestDaemon_ConcurrentStartsOnlyOneServes.
func TestDaemonHelperProcess(t *testing.T) {
	if os.Getenv("SENNIT_DAEMON_HELPER") != "1" {
		return
	}
	projectDir := os.Args[len(os.Args)-1]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		runErr <- daemon.Run(ctx, projectDir, daemon.Options{
			Ready:    func(s string) { fmt.Fprintln(os.Stdout, "ready", s) },
			LogSetup: func(string, bool) {},
		})
	}()

	stdinClosed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(stdinClosed)
	}()

	select {
	case err := <-runErr:
		if err != nil {
			fmt.Fprintln(os.Stdout, "error:", err)
			os.Exit(1)
		}
		os.Exit(0)
	case <-stdinClosed:
		cancel()
		if err := <-runErr; err != nil {
			fmt.Fprintln(os.Stdout, "error:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, "stopped")
		os.Exit(0)
	}
}

// daemonHelper is a running TestDaemonHelperProcess subprocess whose
// first status line has not necessarily been read yet.
type daemonHelper struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

// startDaemonHelperProcess starts a daemon.Run subprocess for
// projectDir without waiting for its outcome -- the caller reads that
// via readLineBounded so two helpers can be started back to back with
// minimal gap between them.
func startDaemonHelperProcess(t *testing.T, ctx context.Context, projectDir string) *daemonHelper {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestDaemonHelperProcess", "--", projectDir)
	cmd.Env = append(os.Environ(), "SENNIT_DAEMON_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	h := &daemonHelper{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}
	t.Cleanup(func() { _ = h.stdin.Close(); _ = h.cmd.Wait() })
	return h
}

// stopAndWait closes h's stdin (its shutdown signal) and waits for the
// process to exit, bounded by ctx.
func (h *daemonHelper) stopAndWait(t *testing.T, ctx context.Context) {
	t.Helper()
	_ = h.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for daemon helper process to exit: %v", ctx.Err())
	}
}

// readLineBounded reads one line from r, failing the test if none
// arrives within timeout rather than hanging it.
func readLineBounded(t *testing.T, r *bufio.Reader, timeout time.Duration) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := r.ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case res := <-ch:
		require.NoError(t, res.err)
		return res.line
	case <-time.After(timeout):
		t.Fatal("timed out waiting for subprocess output")
		return ""
	}
}

// TestDaemon_FailsWhenTUIHoldsLock covers the ordering fix directly: Run
// must acquire the workspace lock (via app.Bootstrap) before it ever
// looks at the socket path. A TUI-mode holder of the same lock -- a
// genuinely separate process, since workspacelock.Acquire shares one
// lock across in-process callers (see the helper-process comment above)
// -- must make Run fail with the lock's own error, and must leave
// whatever was sitting at the socket path completely alone: Run never
// touches a path it isn't certain it exclusively owns.
func TestDaemon_FailsWhenTUIHoldsLock(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)

	projectDir := t.TempDir()
	socketPath := projectSocketPath(t, projectDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(socketPath), 0o700))
	require.NoError(t, os.WriteFile(socketPath, []byte("sentinel"), 0o600))

	lockDir := projectLockDir(t, projectDir)
	// Bootstrap always creates this directory (ensureDataDir) before
	// locking it; the helper process acquires the lock directly, so it
	// needs the same setup done for it here.
	require.NoError(t, os.MkdirAll(lockDir, 0o700))
	startLockHelper(t, lockDir)

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	err := daemon.Run(ctx, projectDir, daemon.Options{LogSetup: func(string, bool) {}})
	require.Error(t, err)
	require.ErrorIs(t, err, workspacelock.ErrLocked)

	data, err := os.ReadFile(socketPath)
	require.NoError(t, err)
	require.Equal(t, "sentinel", string(data), "Run must not touch the socket path when it never held the lock")
}

// TestDaemon_ConcurrentStartsOnlyOneServes is the direct regression test
// for the ordering race described in review: two real `daemon run`
// processes started against the same project as close to simultaneously
// as this harness can manage must never both bind the socket. Exactly
// one wins the workspace lock and ends up serving on the socket path;
// the other fails with the lock's own error, having never touched the
// socket. Run 20 times: the race this guards against is a narrow
// interleaving window inside listen(), not a certainty on any single
// run.
func TestDaemon_ConcurrentStartsOnlyOneServes(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)

	projectDir := t.TempDir()

	for iter := range 20 {
		concurrentStartPair(t, projectDir, iter)
	}
}

// concurrentStartPair starts two real daemon.Run subprocesses for
// projectDir back to back, asserts exactly one becomes ready while the
// other fails with workspacelock.ErrLocked, checks the winner actually
// serves, then shuts the winner down -- all bounded by ctx so a stuck
// child process fails this call (and is killed by exec.CommandContext)
// instead of hanging the test.
func concurrentStartPair(t *testing.T, projectDir string, iter int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	helperA := startDaemonHelperProcess(t, ctx, projectDir)
	helperB := startDaemonHelperProcess(t, ctx, projectDir)

	lineA := readLineBounded(t, helperA.stdout, testTimeout)
	lineB := readLineBounded(t, helperB.stdout, testTimeout)

	readyA, readyB := parseReadyLine(lineA), parseReadyLine(lineB)

	var winner *daemonHelper
	var winnerSocket, loserLine string
	switch {
	case readyA != "" && readyB == "":
		winner, winnerSocket, loserLine = helperA, readyA, lineB
	case readyB != "" && readyA == "":
		winner, winnerSocket, loserLine = helperB, readyB, lineA
	default:
		t.Fatalf("iter %d: expected exactly one helper to become ready (A: %q, B: %q)", iter, lineA, lineB)
	}
	require.Containsf(t, loserLine, workspacelock.ErrLocked.Error(), "iter %d: loser's reported error", iter)

	client, closeClient := dialDaemon(t, winnerSocket)
	_, err := client.Hello(t.Context())
	require.NoErrorf(t, err, "iter %d: winner's socket did not answer Hello", iter)
	closeClient()

	winner.stopAndWait(t, ctx)
}

// parseReadyLine returns the socket path from a "ready <path>\n" line,
// or "" if line isn't one (an "error: ...\n" line, in practice).
func parseReadyLine(line string) string {
	const prefix = "ready "
	if len(line) <= len(prefix) || line[:len(prefix)] != prefix {
		return ""
	}
	return line[len(prefix) : len(line)-1] // trim the trailing '\n'
}

// TestDaemon_ShutdownRPC_AcceptsWhenIdle covers the Meta Shutdown RPC
// (CLIENT-SERVER.md, PR 2.2's version-skew handling): a caller with no
// other connected client and nothing running gets Accepted=true, and the
// daemon actually goes on to shut itself down through the same path idle
// exit uses.
func TestDaemon_ShutdownRPC_AcceptsWhenIdle(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	socketPath, runErr := startDaemon(t, ctx, projectDir)
	client, _ := dialDaemon(t, socketPath)

	accepted, err := client.RequestShutdown(t.Context(), true)
	require.NoError(t, err)
	require.True(t, accepted, "expected an idle daemon to accept a conditional shutdown request")

	awaitShutdown(t, runErr)
}

// TestDaemon_ShutdownRPC_RefusesWhenAnotherClientConnected covers the
// other half: a caller must not see its own connection count as
// busyness (excludingClientCounter), but a second, distinct client's
// still-open connection must -- OnlyIfIdle refuses, and the daemon keeps
// running.
func TestDaemon_ShutdownRPC_RefusesWhenAnotherClientConnected(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	socketPath, runErr := startDaemon(t, ctx, projectDir)
	other, closeOther := dialDaemon(t, socketPath)
	_, err := other.Hello(t.Context())
	require.NoError(t, err)

	caller, closeCaller := dialDaemon(t, socketPath)

	accepted, err := caller.RequestShutdown(t.Context(), true)
	require.NoError(t, err)
	require.False(t, accepted, "expected a conditional shutdown request to be refused while another client is connected")

	closeCaller()
	closeOther()
	cancel()
	awaitShutdown(t, runErr)
}
