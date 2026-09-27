package supervisor_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/testenv"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/daemon"
	"github.com/rave-soft/sennit/internal/daemon/supervisor"
	"github.com/rave-soft/sennit/internal/lock"
	"github.com/rave-soft/sennit/internal/version"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspacelock"
)

// mockGlobalConfig follows AGENTS.md's "Testing without real providers"
// recipe -- a provider that exists only for the test, with the embedded
// catalog disabled, so nothing here can reach a real endpoint.
const mockGlobalConfig = `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`

// testTimeout bounds every EnsureRunning call and every subprocess wait
// this file makes. Widened under -race (raceWait): these tests spawn
// real subprocesses, and -race's own overhead plus CI's cross-package
// CPU contention (AGENTS.md's "wall-clock budgets under -race") can make
// that comfortably slower than in isolation -- a hang guard, not a
// performance assertion.
var testTimeout = raceWait(30 * time.Second)

// raceWait widens a correctness wait/hang-guard budget under -race, the
// same pattern internal/workspace/wsrpc/grpcws uses (see its
// grpcws_test_helpers_test.go doc comment) and internal/daemon uses
// (raceIdleScale). Leave a budget alone when it is itself a performance
// assertion -- see this file's two elapsed-time checks, which skip
// under -race entirely instead (racecheck_off_test.go's doc comment).
func raceWait(d time.Duration) time.Duration {
	if !raceDetectorEnabled {
		return d
	}
	if w := d * 6; w > 60*time.Second {
		return w
	}
	return 60 * time.Second
}

// writeGlobalConfig points the global config location at a fresh
// directory for this test and seeds it with mockGlobalConfig, mirroring
// internal/daemon/daemon_test.go's helper of the same name.
func writeGlobalConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", filepath.Join(dir, "data"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sennit.json"), []byte(mockGlobalConfig), 0o644))
}

// helperCommand returns an Options.Command hook that re-execs this test
// binary as TestSupervisorDaemonHelperProcess -- the same os.Args[0]
// re-exec pattern internal/daemon/daemon_test.go and internal/app/
// bootstrap_test.go use, and for the same reason: EnsureRunning's own
// single-flight is a cross-PROCESS guarantee, so a test proving it needs
// a real second OS process, not a second goroutine in this one.
func helperCommand(t *testing.T) func(args []string) *exec.Cmd {
	t.Helper()
	// SENNIT_SUPERVISOR_DAEMON_HELPER gates TestSupervisorDaemonHelperProcess
	// in the spawned subprocess. It has to be set on THIS (the caller's)
	// process rather than on the *exec.Cmd this closure builds:
	// spawnDetached overwrites cmd.Env wholesale with
	// env.WithoutHerdrEnv(os.Environ()) right after this hook returns,
	// discarding anything set here -- exactly like a real daemon's
	// environment is built from whatever process is doing the spawning.
	t.Setenv("SENNIT_SUPERVISOR_DAEMON_HELPER", "1")
	return func(args []string) *exec.Cmd {
		cwd := extractCwd(args)
		// context.Background(), like production's own spawnDetached:
		// this process is meant to be released and outlive whichever
		// call spawned it, not tied to that call's ctx.
		return exec.CommandContext(context.Background(), os.Args[0], "-test.run=TestSupervisorDaemonHelperProcess", "--", cwd) //nolint:gosec
	}
}

func extractCwd(args []string) string {
	for i, a := range args {
		if a == "--cwd" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestSupervisorDaemonHelperProcess is a subprocess helper (gated by
// SENNIT_SUPERVISOR_DAEMON_HELPER=1) that stands in for a real `sennit
// daemon run` process, spawned by supervisor.spawnDetached through the
// Options.Command hook helperCommand builds. Its behavior is steered by
// a handful of env vars the outer test sets with t.Setenv before calling
// EnsureRunning -- those propagate here because supervisor's own
// spawnDetached builds the child's environment from os.Environ() of
// whichever process is doing the spawning (env.WithoutHerdrEnv(os.Environ())),
// which for these tests is this same re-exec'd test binary.
func TestSupervisorDaemonHelperProcess(t *testing.T) {
	if os.Getenv("SENNIT_SUPERVISOR_DAEMON_HELPER") != "1" {
		return
	}

	// TestMain would normally do this, but it doesn't run for a process
	// invoked with -test.run targeting only this one test function.
	testenvIsolate()

	if p := os.Getenv("SENNIT_TEST_ENV_DUMP"); p != "" {
		_ = os.WriteFile(p, []byte(strings.Join(os.Environ(), "\n")), 0o600)
	}
	if p := os.Getenv("SENNIT_TEST_SPAWN_COUNTER"); p != "" {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintln(f, os.Getpid())
			f.Close()
		}
	}
	if os.Getenv("SENNIT_TEST_NEVER_READY") == "1" {
		// Simulate a daemon that starts but never becomes healthy (a
		// wedged Bootstrap, say): bounded so it cannot leak past a
		// handful of seconds even though nothing here reaps it.
		time.Sleep(3 * time.Second)
		os.Exit(0)
	}
	if v := os.Getenv("SENNIT_TEST_BUILD_ID"); v != "" {
		version.Commit = v
	}

	var delayBeforeListen time.Duration
	if v := os.Getenv("SENNIT_TEST_DELAY_BEFORE_LISTEN"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad SENNIT_TEST_DELAY_BEFORE_LISTEN:", err)
			os.Exit(1)
		}
		delayBeforeListen = d
	}

	cwd := os.Args[len(os.Args)-1]
	if err := daemon.Run(context.Background(), cwd, daemon.Options{
		LogSetup:          func(string, bool) {},
		DelayBeforeListen: delayBeforeListen,
	}); err != nil {
		// Stderr is the startup log supervisor.spawnDetached redirects
		// this whole process's output to (see its doc comment); a real
		// `sennit daemon run` would report this same error through
		// cobra, but this helper otherwise exits 0 either way, which
		// left a real failure here indistinguishable from "never got
		// this far" -- see supervisor.waitForReady's logTail quoting.
		fmt.Fprintln(os.Stderr, "daemon.Run failed:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// testenvIsolate mirrors testenv.IsolateGlobalProfile's HOME/XDG
// redirection for a process that skips TestMain: it only needs to make
// sure nothing here touches the real developer profile, which
// SENNIT_GLOBAL_CONFIG/SENNIT_GLOBAL_DATA (inherited from the parent's
// t.Setenv calls, see writeGlobalConfig) already guarantee. Kept as a
// named no-op rather than removed outright so the reasoning is
// discoverable from the call site above.
func testenvIsolate() {}

// dialAndShutdown connects to socketPath and issues an unconditional
// Meta Shutdown (OnlyIfIdle: false, which this package's daemon side
// always accepts -- see grpcws.WithShutdownHandler's wiring in
// internal/daemon), then waits for the socket to stop answering. Used
// to tear down a real daemon helper this file's tests actually started,
// since supervisor.spawnDetached deliberately releases (rather than
// waits on) the child process it starts -- the daemon's own RPC surface
// is the only reliable, cross-platform way back to it.
func dialAndShutdown(t *testing.T, ctx context.Context, projectDir string) {
	t.Helper()
	socketPath, lockDir, err := daemon.ResolveSocketPath(ctx, projectDir, "", false)
	if err != nil {
		return
	}
	// Read the daemon's real PID before asking it to shut down: spawnDetached
	// releases the child it starts (see its own doc comment), so a real RPC
	// followed by waiting for the PID itself to exit is the only reliable,
	// cross-platform way to know it is actually gone -- see
	// testenv.WaitForProcessExit's doc comment for why this matters.
	owner, ok, _ := workspacelock.CurrentOwner(lockDir)

	opts := append(grpcws.DefaultClientDialOptions(),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	conn, err := grpc.NewClient("passthrough:///"+socketPath, opts...)
	if err != nil {
		return
	}
	defer conn.Close()
	client := grpcws.NewClient(conn)
	defer client.Shutdown()

	_, _ = client.RequestShutdown(ctx, false)

	deadline := time.Now().Add(raceWait(10 * time.Second))
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); os.IsNotExist(err) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}

	if ok {
		testenv.WaitForProcessExit(t, owner.PID, raceWait(10*time.Second))
	}
}

// TestEnsureRunning_ColdStart covers the basic contract: no daemon is
// running, EnsureRunning spawns exactly one, and the returned socket
// answers Hello.
func TestEnsureRunning_ColdStart(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	socketPath, warning, err := supervisor.EnsureRunning(ctx, projectDir, supervisor.Options{Command: helperCommand(t)})
	require.NoError(t, err)
	require.Empty(t, warning)
	require.NotEmpty(t, socketPath)
	t.Cleanup(func() { dialAndShutdown(t, context.Background(), projectDir) })

	opts := append(grpcws.DefaultClientDialOptions(),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	conn, err := grpc.NewClient("passthrough:///"+socketPath, opts...)
	require.NoError(t, err)
	defer conn.Close()
	client := grpcws.NewClient(conn)
	defer client.Shutdown()
	hello, err := client.Hello(t.Context())
	require.NoError(t, err)
	require.NotZero(t, hello.ProtocolVersion)
}

// TestEnsureRunning_ConcurrentCallersSpawnOnlyOneDaemon is the direct
// regression test for the single-flight guarantee (CLIENT-SERVER.md, PR
// 2.2; mirrors the pre-C1 clientserverrace/race_test.go): 8 concurrent,
// independent EnsureRunning calls against the same project must produce
// exactly one daemon process, and every caller must get the same
// socket.
func TestEnsureRunning_ConcurrentCallersSpawnOnlyOneDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("spawn counter file locking assumed here is unix-specific")
	}

	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)

	spawnCounter := filepath.Join(t.TempDir(), "spawn-counter")
	t.Setenv("SENNIT_TEST_SPAWN_COUNTER", spawnCounter)

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	// Built once, outside the goroutines below: t.Setenv (which
	// helperCommand calls) panics on concurrent use from a test that
	// isn't marked parallel, since it mutates shared *testing.T state.
	command := helperCommand(t)

	const numCallers = 8
	sockets := make([]string, numCallers)
	errs := make([]error, numCallers)
	var wg sync.WaitGroup
	for i := range numCallers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sockets[i], _, errs[i] = supervisor.EnsureRunning(ctx, projectDir, supervisor.Options{Command: command})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "caller %d", i)
		require.Equalf(t, sockets[0], sockets[i], "caller %d got a different socket", i)
	}
	t.Cleanup(func() { dialAndShutdown(t, context.Background(), projectDir) })

	data, err := os.ReadFile(spawnCounter)
	require.NoError(t, err)
	pids := strings.Fields(string(data))
	require.Lenf(t, pids, 1, "expected exactly one daemon process spawned, got pids %v", pids)
}

// TestEnsureRunning_StaleSocketAfterCrash covers the kill-9 case: a
// daemon dies without releasing anything cleanly at the OS level, but
// the workspace lock's OS flock (unlike its on-disk record) *is*
// released by the kernel on process exit. EnsureRunning must treat that
// as "not running" and start a fresh daemon rather than getting stuck
// on the stale record.
func TestEnsureRunning_StaleSocketAfterCrash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGKILL-based crash simulation is unix-specific")
	}

	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	socketPath1, _, err := supervisor.EnsureRunning(ctx, projectDir, supervisor.Options{Command: helperCommand(t)})
	require.NoError(t, err)

	pid := findDaemonPID(t, ctx, projectDir)
	require.NoError(t, killDaemon(pid))
	waitGone(t, ctx, socketPath1)

	socketPath2, _, err := supervisor.EnsureRunning(ctx, projectDir, supervisor.Options{Command: helperCommand(t)})
	require.NoError(t, err)
	t.Cleanup(func() { dialAndShutdown(t, context.Background(), projectDir) })

	require.Equal(t, socketPath1, socketPath2, "the daemon's socket is deterministic per project")

	pid2 := findDaemonPID(t, ctx, projectDir)
	require.NotEqual(t, pid, pid2, "expected a genuinely new daemon process")
}

// TestEnsureRunning_TUIHoldsLock covers ErrTUILocked: EnsureRunning must
// refuse to spawn a competing daemon when the project's workspace lock
// is held by an embedded (TUI-mode) process, and its error must name
// that process's PID and unwrap to workspacelock.ErrLocked.
//
// The TUI holder has to be a genuinely separate OS process: workspacelock.
// Acquire deliberately shares one OS lock across concurrent in-process
// acquirers for the same directory (poolEntry refcounting -- see
// workspacelock.go's own doc comment, and internal/daemon/daemon_test.go's
// identical note on TestDaemonLockHelperProcess), so calling Acquire a
// second time from THIS test process would just refcount the same lock
// rather than reproduce the contention EnsureRunning needs to detect.
func TestEnsureRunning_TUIHoldsLock(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	lockDir, err := daemonResolveLockDir(ctx, projectDir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(lockDir, 0o700))

	helper := startLockHelper(t, lockDir)
	defer helper.stop()

	start := time.Now()
	_, _, err = supervisor.EnsureRunning(ctx, projectDir, supervisor.Options{Command: helperCommand(t)})
	elapsed := time.Since(start)

	require.Error(t, err)
	require.ErrorIs(t, err, workspacelock.ErrLocked)
	var tuiErr *supervisor.ErrTUILocked
	require.ErrorAsf(t, err, &tuiErr, "expected *supervisor.ErrTUILocked, got %T: %v", err, err)
	require.Equal(t, helper.pid, tuiErr.PID)
	// A genuine TUI lock is unambiguous the instant EnsureRunning reads
	// it (ModeTUI + a live flock): it must not wait out any part of the
	// readiness timeout to report it. defaultReadyTimeout is 10s; this
	// generously bounds "fast" at a small fraction of that.
	//
	// Skipped under -race rather than widened: this is a performance
	// assertion (it must NOT take as long as the readiness timeout), and
	// -race's own instrumentation overhead can blow a fixed "fast" bound
	// on its own, independent of whether EnsureRunning's actual behavior
	// is correct (see AGENTS.md's wall-clock-budgets-under-race note and
	// racecheck_off_test.go's doc comment).
	if raceDetectorEnabled {
		t.Skip("elapsed-time assertion is not meaningful under -race; see racecheck_off_test.go")
	}
	require.Lessf(t, elapsed, 2*time.Second, "expected ErrTUILocked without waiting for the readiness timeout, took %s", elapsed)
}

// TestEnsureRunning_SlowBootstrapIsNotMistakenForTUI is the direct
// regression test for the bug tuiAmbiguityGrace used to paper over:
// app.Bootstrap acquires the workspace lock before a daemon's socket is
// bound, and a slow Bootstrap (cold MCP/LSP init, a loaded CI runner,
// -race, Windows) must never be misreported as "a TUI holds this lock."
// The fix is structural, not timing-based: daemon.Run now acquires the
// lock already in ModeDaemon (see app.BootstrapOptions.WorkspaceLockMode),
// so EnsureRunning can tell "a daemon is starting" from "a TUI is here"
// by Mode alone, regardless of how long Bootstrap takes.
//
// A single EnsureRunning call would not actually exercise this: the
// caller that itself wins the spawn commits to spawnDetached+waitForReady
// and never re-reads Mode again, so it would succeed once the socket
// comes up regardless of what Mode was recorded in between (waitForReady
// only polls health). The bug only shows up for a SECOND, concurrent
// caller that reads the lock file while the first daemon is still mid-
// Bootstrap (Mode recorded, socket not yet bound) -- exactly
// probeRunning's own "ModeDaemon with an empty Socket" branch. So this
// starts a second EnsureRunning call shortly after the first, timed to
// land while the spawned helper is still inside its (artificially
// slowed) DelayBeforeListen window, and requires both to succeed with
// the same socket rather than the second one seeing ErrTUILocked.
func TestEnsureRunning_SlowBootstrapIsNotMistakenForTUI(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)
	t.Setenv("SENNIT_TEST_DELAY_BEFORE_LISTEN", "3s")

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	command := helperCommand(t)
	opts := supervisor.Options{Command: command, ReadyTimeout: 10 * time.Second}

	var wg sync.WaitGroup
	var sockets [2]string
	var errs [2]error

	wg.Add(1)
	go func() {
		defer wg.Done()
		sockets[0], _, errs[0] = supervisor.EnsureRunning(ctx, projectDir, opts)
	}()

	// Give the first call time to spawn the helper and for its
	// app.Bootstrap to acquire the workspace lock (ModeDaemon, empty
	// socket), while remaining well inside the 3s DelayBeforeListen
	// window this test set above.
	time.Sleep(750 * time.Millisecond)

	wg.Add(1)
	go func() {
		defer wg.Done()
		sockets[1], _, errs[1] = supervisor.EnsureRunning(ctx, projectDir, opts)
	}()

	wg.Wait()
	for i, err := range errs {
		require.NoErrorf(t, err, "caller %d", i)
	}
	require.Equal(t, sockets[0], sockets[1])
	t.Cleanup(func() { dialAndShutdown(t, context.Background(), projectDir) })
}

// TestSupervisorLockHelperProcess is a subprocess helper (gated by
// SENNIT_SUPERVISOR_LOCK_HELPER=1) that acquires the workspace lock at
// the directory given as its last argument (in the default ModeTUI) and
// holds it until stdin closes -- simulating an interactive TUI already
// holding a project's workspace lock in a genuinely separate process.
// Mirrors internal/daemon/daemon_test.go's TestDaemonLockHelperProcess.
func TestSupervisorLockHelperProcess(t *testing.T) {
	if os.Getenv("SENNIT_SUPERVISOR_LOCK_HELPER") != "1" {
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
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	os.Exit(0)
}

// lockHelper is a running TestSupervisorLockHelperProcess subprocess.
type lockHelper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	pid   int
}

// startLockHelper starts a lock-holding subprocess for lockDir and
// blocks until it reports the lock acquired.
func startLockHelper(t *testing.T, lockDir string) *lockHelper {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestSupervisorLockHelperProcess", "--", lockDir) //nolint:gosec
	cmd.Env = append(os.Environ(), "SENNIT_SUPERVISOR_LOCK_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	h := &lockHelper{cmd: cmd, stdin: stdin, pid: cmd.Process.Pid}
	t.Cleanup(h.stop)

	line := readLineBounded(t, bufio.NewReader(stdout), testTimeout)
	require.Equal(t, "locked\n", line)
	return h
}

func (h *lockHelper) stop() {
	_ = h.stdin.Close()
	_ = h.cmd.Wait()
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

// TestEnsureRunning_StripsHerdrEnv covers the daemon-env requirement in
// CLIENT-SERVER.md's PR 1.5/2.2: a daemon spawned by the supervisor must
// never inherit HERDR_* variables, or it would permanently attach to
// whichever terminal pane happened to spawn it first.
func TestEnsureRunning_StripsHerdrEnv(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/should-not-be-inherited.sock")
	t.Setenv("HERDR_PANE_ID", "pane-1")

	envDump := filepath.Join(t.TempDir(), "env-dump")
	t.Setenv("SENNIT_TEST_ENV_DUMP", envDump)

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	_, _, err := supervisor.EnsureRunning(ctx, projectDir, supervisor.Options{Command: helperCommand(t)})
	require.NoError(t, err)
	t.Cleanup(func() { dialAndShutdown(t, context.Background(), projectDir) })

	data, err := os.ReadFile(envDump)
	require.NoError(t, err)
	for _, key := range []string{"HERDR_ENV=", "HERDR_SOCKET_PATH=", "HERDR_PANE_ID="} {
		require.NotContainsf(t, string(data), key, "daemon environment must not carry %s", key)
	}
}

// TestEnsureRunning_ReadinessTimeout covers a daemon helper that starts
// but never becomes healthy: EnsureRunning must give up within its
// (shrunk, for this test) ReadyTimeout budget rather than hang, and the
// error must quote the startup log.
func TestEnsureRunning_ReadinessTimeout(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)
	t.Setenv("SENNIT_TEST_NEVER_READY", "1")

	projectDir := t.TempDir()
	t.Cleanup(func() { testenv.AssertRemovableOnWindows(t, projectDir) })
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	// TestSupervisorDaemonHelperProcess's own SENNIT_TEST_NEVER_READY path
	// sleeps 3s and exits on its own -- self-bounded so nothing here
	// leaks a process past that, but EnsureRunning gives up long before
	// that on its own 500ms ReadyTimeout and returns while the child is
	// still very much alive, still holding this project's startup log
	// open. On Windows that open handle is what makes projectDir's own
	// t.TempDir() cleanup (which runs right after this test returns) fail
	// with "used by another process" -- so wait for the child to actually
	// exit before returning, the same way dialAndShutdown does for a
	// daemon that did come up.
	//
	// spawnDetached itself releases (os.Process.Release) the *exec.Cmd it
	// starts before EnsureRunning ever gets a chance to return -- Release
	// sets Process.Pid to -1, so reading it back from a Command hook
	// after the fact (rather than before Release runs) always sees -1,
	// not the real pid. SENNIT_TEST_SPAWN_COUNTER, already used by
	// TestEnsureRunning_ConcurrentCallersSpawnOnlyOneDaemon for the same
	// reason, sidesteps that: the pid it records is the one the helper
	// process reports about itself (os.Getpid()), written before its own
	// NEVER_READY sleep.
	spawnCounter := filepath.Join(t.TempDir(), "spawn-counter")
	t.Setenv("SENNIT_TEST_SPAWN_COUNTER", spawnCounter)

	start := time.Now()
	_, _, err := supervisor.EnsureRunning(ctx, projectDir, supervisor.Options{
		Command:      helperCommand(t),
		ReadyTimeout: 500 * time.Millisecond,
		ProbeTimeout: 50 * time.Millisecond,
	})
	elapsed := time.Since(start)

	require.Error(t, err)
	require.ErrorContains(t, err, "timed out waiting for daemon to become ready")
	// Skipped under -race rather than widened: a fixed 500ms ReadyTimeout
	// racing -race's own instrumentation overhead is not a meaningful
	// performance assertion any more (see racecheck_off_test.go's doc
	// comment); the error path itself (asserted above) is what this test
	// is really for, and that already ran.
	if raceDetectorEnabled {
		t.Skip("elapsed-time assertion is not meaningful under -race; see racecheck_off_test.go")
	}
	require.Lessf(t, elapsed, 5*time.Second, "expected the bounded ReadyTimeout to be honored, took %s", elapsed)

	testenv.WaitForProcessExit(t, readSpawnedPID(t, spawnCounter), raceWait(10*time.Second))
}

// readSpawnedPID reads back the single pid TestSupervisorDaemonHelperProcess
// wrote to path (via SENNIT_TEST_SPAWN_COUNTER) about itself.
func readSpawnedPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "helper process never recorded its own pid")
	line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	pid, err := strconv.Atoi(line)
	require.NoErrorf(t, err, "malformed spawn counter contents %q", data)
	return pid
}

// TestEnsureRunning_BuildMismatch_RestartsWhenIdle covers version-skew
// handling (CLIENT-SERVER.md, PR 2.2): a running daemon reporting a
// different BuildID than this client, while idle, is restarted, and the
// caller gets a fresh daemon back with no warning.
func TestEnsureRunning_BuildMismatch_RestartsWhenIdle(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)
	t.Setenv("SENNIT_TEST_BUILD_ID", "stale-build")

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	staleSocket, _, err := supervisor.EnsureRunning(ctx, projectDir, supervisor.Options{Command: helperCommand(t)})
	require.NoError(t, err)
	stalePID := findDaemonPID(t, ctx, projectDir)

	// This client's own version.Commit differs from "stale-build"
	// (whatever the default devel value is), so a second EnsureRunning
	// call -- this time with a helper that reports the real BuildID --
	// must observe the mismatch and restart.
	require.NoError(t, os.Unsetenv("SENNIT_TEST_BUILD_ID"))

	freshSocket, warning, err := supervisor.EnsureRunning(ctx, projectDir, supervisor.Options{Command: helperCommand(t)})
	require.NoError(t, err)
	require.Empty(t, warning)
	t.Cleanup(func() { dialAndShutdown(t, context.Background(), projectDir) })

	require.Equal(t, staleSocket, freshSocket, "the socket path is deterministic per project")
	freshPID := findDaemonPID(t, ctx, projectDir)
	require.NotEqual(t, stalePID, freshPID, "expected the stale-build daemon to be replaced")
}

// TestEnsureRunning_BuildMismatch_WarnsWhenBusy covers the other half:
// a busy daemon (another client connected to it) with a differing
// BuildID must be reused, with a warning EnsureRunning's caller can
// print, rather than torn down mid-use.
func TestEnsureRunning_BuildMismatch_WarnsWhenBusy(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)
	t.Setenv("SENNIT_TEST_BUILD_ID", "stale-build")

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	staleSocket, _, err := supervisor.EnsureRunning(ctx, projectDir, supervisor.Options{Command: helperCommand(t)})
	require.NoError(t, err)
	t.Cleanup(func() { dialAndShutdown(t, context.Background(), projectDir) })

	// Hold a connection open so the daemon looks busy to its own
	// OnlyIfIdle check (excludingClientCounter, internal/daemon) when
	// the next call's Shutdown request comes from a DIFFERENT client.
	opts := append(grpcws.DefaultClientDialOptions(),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", staleSocket)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	conn, err := grpc.NewClient("passthrough:///"+staleSocket, opts...)
	require.NoError(t, err)
	defer conn.Close()
	busyClient := grpcws.NewClient(conn)
	require.NoError(t, busyClient.Connect(t.Context()))
	defer busyClient.Shutdown()

	require.NoError(t, os.Unsetenv("SENNIT_TEST_BUILD_ID"))

	socketPath, warning, err := supervisor.EnsureRunning(ctx, projectDir, supervisor.Options{Command: helperCommand(t)})
	require.NoError(t, err)
	require.Equal(t, staleSocket, socketPath, "expected the busy, mismatched daemon to be reused rather than replaced")
	require.NotEmpty(t, warning)
	require.Contains(t, warning, "stale-build")
}

// findDaemonPID reads the workspace lock's owner PID for projectDir --
// deliberately via the lock file, not by dialing the daemon and asking:
// an RPC connection registers a lease (see excludingClientCounter's own
// doc comment) that lingers for grpcws's defaultHandleLeaseGrace (10s)
// after it closes, which would make a test that calls this and then
// immediately checks OnlyIfIdle behavior see a false "busy".
func findDaemonPID(t *testing.T, ctx context.Context, projectDir string) int {
	t.Helper()
	lockDir, err := daemonResolveLockDir(ctx, projectDir)
	require.NoError(t, err)
	info, ok, err := workspacelock.CurrentOwner(lockDir)
	require.NoError(t, err)
	require.True(t, ok)
	return info.PID
}

// daemonResolveLockDir is daemon.ResolveSocketPath's lockDir half, for a
// test that only needs the lock directory (to read owner info) rather
// than the socket path itself.
func daemonResolveLockDir(ctx context.Context, projectDir string) (string, error) {
	_, lockDir, err := daemon.ResolveSocketPath(ctx, projectDir, "", false)
	return lockDir, err
}

// killDaemon sends SIGKILL, simulating a crash the daemon cannot clean
// up after (no graceful shutdown, no lock release beyond what the
// kernel does automatically).
func killDaemon(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}

// waitGone polls until nothing answers a health-independent liveness
// check on socketPath: plain connect-and-close, since the process is
// dead and nothing is listening at all (as opposed to listening-but-
// unhealthy, which probeHealthy inside the package already covers).
func waitGone(t *testing.T, ctx context.Context, socketPath string) {
	t.Helper()
	deadline := time.Now().Add(raceWait(10 * time.Second))
	for time.Now().Before(deadline) {
		dialCtx, dialCancel := context.WithTimeout(ctx, 100*time.Millisecond)
		conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", socketPath)
		dialCancel()
		if err != nil {
			return
		}
		conn.Close()
		select {
		case <-ctx.Done():
			t.Fatal("context done while waiting for killed daemon's socket to go quiet")
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("timed out waiting for killed daemon's socket to stop answering")
}

// TestAwaitWorkspaceLockFree_WaitsForRelease pins the gap AwaitGone alone
// leaves open (see AwaitWorkspaceLockFree's own doc comment): a socket
// going quiet doesn't mean the workspace lock is free yet. This holds
// the lock file directly (as if a shutting-down daemon still held it),
// confirms AwaitWorkspaceLockFree does not return early, then releases
// it and confirms AwaitWorkspaceLockFree unblocks.
func TestAwaitWorkspaceLockFree_WaitsForRelease(t *testing.T) {
	t.Parallel()

	lockDir := t.TempDir()
	release, err := lock.TryFile(filepath.Join(lockDir, brand.LockFile))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), raceWait(5*time.Second))
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- supervisor.AwaitWorkspaceLockFree(ctx, lockDir) }()

	select {
	case err := <-done:
		t.Fatalf("AwaitWorkspaceLockFree returned %v while the lock was still held", err)
	case <-time.After(200 * time.Millisecond):
	}

	release()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(raceWait(5 * time.Second)):
		t.Fatal("AwaitWorkspaceLockFree did not unblock after the lock was released")
	}
}

// TestAwaitWorkspaceLockFree_CtxDone covers the other side: a lock that
// never frees must not hang past ctx's own deadline.
func TestAwaitWorkspaceLockFree_CtxDone(t *testing.T) {
	t.Parallel()

	lockDir := t.TempDir()
	release, err := lock.TryFile(filepath.Join(lockDir, brand.LockFile))
	require.NoError(t, err)
	t.Cleanup(release)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err = supervisor.AwaitWorkspaceLockFree(ctx, lockDir)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
