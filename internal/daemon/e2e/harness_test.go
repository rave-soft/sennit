package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/rave-soft/sennit/internal/daemon"
	"github.com/rave-soft/sennit/internal/daemon/supervisor"
	"github.com/rave-soft/sennit/internal/env"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
)

// hangGuard bounds every wait this package's tests make for the real
// subprocess to do something (become ready, exit, answer an RPC) --
// widened under -race is moot here since the whole package skips under
// -race (racecheck_off_test.go), but the budgets are generous regardless:
// a real `go build` and a real fork/exec cost real wall time even outside
// -race.
const hangGuard = 20 * time.Second

// mockProviderConfig is the AGENTS.md "Testing without real providers"
// recipe, pointed at a fixture server this test starts itself -- nothing
// this package runs can reach a real LLM endpoint.
func mockProviderConfig(fixtureURL string) string {
	return fmt.Sprintf(`{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": %q, "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`, fixtureURL+"/v1")
}

// setupGlobalProfile points SENNIT_GLOBAL_CONFIG/SENNIT_GLOBAL_DATA and
// XDG_RUNTIME_DIR at a fresh directory for this test (t.Setenv, so this
// process and any subprocess it launches -- which inherits os.Environ() --
// resolve the exact same paths; daemon.ResolveSocketPath, called from this
// test process below, has to agree with what the subprocess itself binds
// to) and seeds the global profile with a sennit.json naming the mock
// provider fixtureURL serves.
func setupGlobalProfile(t *testing.T, fixtureURL string) {
	t.Helper()
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "sennit.json"), []byte(mockProviderConfig(fixtureURL)), 0o644))
	t.Setenv("SENNIT_GLOBAL_CONFIG", configDir)
	t.Setenv("SENNIT_GLOBAL_DATA", filepath.Join(dir, "data"))
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
}

// writeShortIdleTimeoutConfig writes projectDir/sennit.json setting
// options.daemon.idle_timeout to a value short enough for these tests to
// observe within a normal test run (real seconds, not production's 10m
// default) -- the project-scoped layer a daemon for that directory loads;
// never providers/model, which are global-only (see AGENTS.md).
func writeShortIdleTimeoutConfig(t *testing.T, projectDir string) {
	t.Helper()
	const cfgJSON = `{"options":{"daemon":{"idle_timeout":"1s"}}}`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "sennit.json"), []byte(cfgJSON), 0o644))
}

// gitInitRepo turns dir into a git repository with one commit, the
// minimum a worktree scenario needs: EnterWorktree resolves the current
// branch (git.CurrentBranch) before creating a worktree off it, which
// fails on a repo with no commits at all.
func gitInitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // fixed argv, test-only.
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %v: %s", args, out)
	}
	run("init")
	run("config", "user.email", "e2e@example.com")
	run("config", "user.name", "sennit e2e")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("e2e fixture repo\n"), 0o644))
	run("add", "README.md")
	run("commit", "-m", "initial commit")
}

// daemonProcess is one `sennit daemon run` subprocess this package
// launched directly (not through supervisor.EnsureRunning -- these tests
// need to kill -9 it, watch it exit on its own from idle, and control its
// env/args precisely). socketPath is resolved independently up front
// (daemon.ResolveSocketPath), the same way a real client would, rather
// than parsed out of the child's stdout.
type daemonProcess struct {
	t          *testing.T
	cmd        *exec.Cmd
	socketPath string
	projectDir string
	out        *syncBuffer
	// waitCh receives cmd.Wait's result exactly once, from the
	// background goroutine startDaemonProcess starts right after
	// cmd.Start: cmd.Wait must only ever be called once, and several
	// scenarios need to both poll "has it exited yet" (readiness, kill
	// -9 detection) and later block for the real exit code, so both
	// waitExit and the readiness loop read this channel instead of
	// calling Wait themselves.
	waitCh chan error
}

// syncBuffer is a bytes.Buffer safe for concurrent writes (the child's
// stdout/stderr) and reads (a test quoting it on failure).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startDaemonProcess launches `sennit daemon run --cwd projectDir` as a
// real subprocess against the built binary (sennitBinary, from
// main_test.go), with its own isolated global profile (fixtureURL feeds
// the mock provider) and runtime dir (so its socket path is confined to
// this test), and waits for it to answer a gRPC health check before
// returning. It does not register any cleanup beyond killing the process;
// callers that need graceful shutdown call requestShutdown or send
// SIGTERM themselves and then wait, since several scenarios need control
// over exactly how the daemon goes down (kill -9 vs. graceful).
//
// Calling this twice in the same test would call setupGlobalProfile twice,
// handing the second daemon a brand new (empty) global profile -- and
// with it a brand new sessions/messages database, since that lives under
// the global data directory, not the project directory (see
// internal/app/bootstrap.go's db.Connect(ctx, globalDBDir)). A restart
// scenario that needs the second daemon to see the first one's sessions
// calls setupGlobalProfile itself, once, and then startDaemonProcessNamed
// for each start.
func startDaemonProcess(t *testing.T, projectDir, fixtureURL string) *daemonProcess {
	t.Helper()
	setupGlobalProfile(t, fixtureURL)
	return startDaemonProcessNamed(t, projectDir)
}

// startDaemonProcessNamed is startDaemonProcess without the profile setup,
// for a test that needs to start more than one daemon (in succession)
// against the same global profile -- see startDaemonProcess's doc comment.
func startDaemonProcessNamed(t *testing.T, projectDir string) *daemonProcess {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), hangGuard)
	defer cancel()
	socketPath, _, err := daemon.ResolveSocketPath(ctx, projectDir, "", false)
	require.NoError(t, err)

	args := []string{"daemon", "run", "--cwd", projectDir, "--trust-project"}
	// context.Background(), not ctx above (already canceled by the defer
	// right after it's used for ResolveSocketPath): this process has to
	// outlive startDaemonProcessNamed's own return, the same reasoning
	// internal/daemon/supervisor's spawnDetached gives for the same
	// choice. This package controls the child's lifetime explicitly
	// (killNow, shutdownCleanly, the Cleanup backstop), never through
	// context cancellation.
	cmd := exec.CommandContext(context.Background(), sennitBinary, args...) //nolint:gosec // sennitBinary is our own freshly-built binary.
	cmd.Env = env.WithoutHerdrEnv(os.Environ())
	out := &syncBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	require.NoError(t, cmd.Start())

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	dp := &daemonProcess{t: t, cmd: cmd, socketPath: socketPath, projectDir: projectDir, out: out, waitCh: waitCh}
	// Backstop: a test that forgets to shut the daemon down cleanly (or
	// fails before reaching its own cleanup) must not leave a live
	// `daemon run` process behind for pgrep to still find once the suite
	// exits (acceptance criteria). Process.Kill is portable (SIGKILL on
	// unix, TerminateProcess on Windows); Signal(-9) is not.
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	probeCtx, probeCancel := context.WithTimeout(context.Background(), hangGuard)
	defer probeCancel()
	deadline := time.Now().Add(hangGuard)
	for !supervisor.ProbeHealthy(probeCtx, socketPath, 500*time.Millisecond) {
		select {
		case err := <-waitCh:
			// Put it back so a later waitExit/requireGone still observes
			// it -- this channel is buffered to exactly one send.
			waitCh <- err
			t.Fatalf("daemon exited before becoming healthy (err=%v); output:\n%s", err, out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never became healthy at %s; output:\n%s", socketPath, out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	return dp
}

// dial connects a fresh *grpcws.Client to dp's socket, exactly as a real
// frontend does (grpcws.DefaultClientDialOptions over a real unix
// socket). The returned close func is idempotent and also registered
// with t.Cleanup as a backstop.
func (dp *daemonProcess) dial(t *testing.T) (client *grpcws.Client, closeClient func()) {
	t.Helper()
	dialOpts := append(grpcws.DefaultClientDialOptions(),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", dp.socketPath)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	conn, err := grpc.NewClient("passthrough:///"+dp.socketPath, dialOpts...)
	require.NoError(t, err)

	client = grpcws.NewClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), hangGuard)
	defer cancel()
	require.NoError(t, client.Connect(ctx))
	closeClient = func() {
		client.Shutdown()
		_ = conn.Close()
	}
	t.Cleanup(closeClient)
	return client, closeClient
}

// waitExit waits up to hangGuard for the daemon's process to exit
// (however it was told to -- SIGTERM, RequestShutdown, idle, or killed by
// the caller) and reports its error, mirroring cmd.Wait's contract.
func (dp *daemonProcess) waitExit(t *testing.T) error {
	t.Helper()
	select {
	case err := <-dp.waitCh:
		return err
	case <-time.After(hangGuard):
		t.Fatalf("daemon did not exit within %s; output:\n%s", hangGuard, dp.out.String())
		return nil
	}
}

// stillRunning reports whether the daemon process is (as far as this
// process can tell without blocking) still alive and answering its
// health check -- used by tests asserting the daemon did NOT exit yet
// (idle survival while busy), where a false negative would silently pass
// a broken idle check.
func (dp *daemonProcess) stillRunning(t *testing.T) bool {
	t.Helper()
	select {
	case err := <-dp.waitCh:
		dp.waitCh <- err // put it back for a later waitExit/requireGone.
		return false
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return supervisor.ProbeHealthy(ctx, dp.socketPath, 500*time.Millisecond)
}

// shutdownCleanly asks the daemon to stop (RequestShutdown, forced rather
// than onlyIfIdle -- the caller is done exercising it, whether or not it
// still looks busy) and waits for the process to actually exit. Tests
// that leave a daemon running past their own end make it that much
// longer before its still-open HTTP connection to the test's fixture
// server lets httptest.Server.Close return (it force-closes after 5s
// regardless, but this avoids the wait and the scary-looking log line).
func shutdownCleanly(t *testing.T, dp *daemonProcess, client *grpcws.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), hangGuard)
	defer cancel()
	_, err := client.RequestShutdown(ctx, false)
	require.NoError(t, err)
	require.NoError(t, dp.waitExit(t))
	dp.requireGone(t)
}

// killNow terminates the daemon process immediately and unconditionally
// -- Process.Kill, which is SIGKILL on unix and TerminateProcess on
// Windows, so the interrupted-turn recovery scenario (d) exercises the
// same "no graceful shutdown ran at all" condition finalizeInterruptedTurns
// exists for, on every platform this package runs on.
func (dp *daemonProcess) killNow(t *testing.T) {
	t.Helper()
	require.NoError(t, dp.cmd.Process.Kill())
}

// requireGone polls until nothing answers dp.socketPath and the socket
// file itself is removed, or fails the test after hangGuard.
func (dp *daemonProcess) requireGone(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), hangGuard)
	defer cancel()
	require.NoError(t, supervisor.AwaitGone(ctx, dp.socketPath))
	_, err := os.Stat(dp.socketPath)
	require.True(t, os.IsNotExist(err), "expected socket file to be removed")
}
