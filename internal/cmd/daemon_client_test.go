package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/daemon"
	"github.com/rave-soft/sennit/internal/daemon/supervisor"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspacelock"
)

// mockGlobalConfig follows AGENTS.md's "Testing without real providers"
// recipe: a provider that exists only for this test, with the embedded
// catalog disabled, so a daemon this file spawns can never reach a real
// endpoint.
const mockGlobalConfig = `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`

// daemonTestTimeout bounds every setupDaemonWorkspace call and subprocess
// wait in this file. Widened under -race (raceWait): these tests spawn
// real subprocesses and dial real unix sockets, and -race's own overhead
// plus CI's cross-package CPU contention (AGENTS.md's "wall-clock budgets
// under -race") can make that comfortably slower than in isolation -- a
// hang guard, not a performance assertion.
var daemonTestTimeout = raceWait(30 * time.Second)

// writeGlobalConfig points the global config location at a fresh
// directory for this test and seeds it with mockGlobalConfig, mirroring
// internal/daemon/supervisor's test helper of the same name. It
// overrides this package's TestMain-wide isolation with its own
// directory, so it must not run under t.Parallel with another test doing
// the same.
func writeGlobalConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", filepath.Join(dir, "data"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sennit.json"), []byte(mockGlobalConfig), 0o644))
}

// helperCommand returns a supervisor.Options.Command hook that re-execs
// this test binary as TestCmdDaemonHelperProcess, standing in for a real
// `sennit daemon run` process -- the same pattern
// internal/daemon/supervisor's own tests use, and for the same reason:
// setupDaemonWorkspace's EnsureRunning call needs a real second OS
// process to find, not a fake in this one.
func helperCommand(t *testing.T) func(args []string) *exec.Cmd {
	t.Helper()
	t.Setenv("SENNIT_CMD_DAEMON_HELPER", "1")
	return func(args []string) *exec.Cmd {
		cwd := extractCwdFlag(args)
		return exec.CommandContext(context.Background(), os.Args[0], "-test.run=TestCmdDaemonHelperProcess", "--", cwd) //nolint:gosec
	}
}

func extractCwdFlag(args []string) string {
	for i, a := range args {
		if a == "--cwd" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestCmdDaemonHelperProcess is a subprocess helper (gated by
// SENNIT_CMD_DAEMON_HELPER=1) that stands in for a real `sennit daemon
// run` process.
func TestCmdDaemonHelperProcess(t *testing.T) {
	if os.Getenv("SENNIT_CMD_DAEMON_HELPER") != "1" {
		return
	}
	cwd := os.Args[len(os.Args)-1]
	_ = daemon.Run(context.Background(), cwd, daemon.Options{
		LogSetup: func(string, bool) {},
	})
	os.Exit(0)
}

// TestCmdLockHelperProcess is a subprocess helper (gated by
// SENNIT_CMD_LOCK_HELPER=1) that acquires the workspace lock at the
// directory given as its last argument in the default ModeTUI and holds
// it until stdin closes -- simulating an interactive TUI already holding
// a project's workspace lock in a genuinely separate process (same-
// process double-acquire does not contend, since flock's lock is scoped
// to the open file description, not the process).
func TestCmdLockHelperProcess(t *testing.T) {
	if os.Getenv("SENNIT_CMD_LOCK_HELPER") != "1" {
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

type lockHelper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	pid   int
}

func startLockHelper(t *testing.T, lockDir string) *lockHelper {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestCmdLockHelperProcess", "--", lockDir) //nolint:gosec
	cmd.Env = append(os.Environ(), "SENNIT_CMD_LOCK_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	h := &lockHelper{cmd: cmd, stdin: stdin, pid: cmd.Process.Pid}
	t.Cleanup(func() {
		_ = h.stdin.Close()
		_ = h.cmd.Wait()
	})

	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "locked\n", line)
	return h
}

// TestSetupDaemonWorkspace_ConnectsAndWorks covers the happy path: a
// fresh daemon is spawned, setupDaemonWorkspace returns a connected
// *grpcws.Client, a unary call against it works, and the prefs store it
// returns writes to the client's own (local) global config file.
func TestSetupDaemonWorkspace_ConnectsAndWorks(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	command := helperCommand(t)

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()

	client, prefs, cleanup, err := setupDaemonWorkspace(ctx, projectDir, "", false, supervisor.Options{Command: command})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	// cleanup (above) only ends this client's own connection, by design
	// (CLIENT-SERVER.md, PR 2.3) -- the spawned daemon itself is torn
	// down separately, the same way supervisor's own tests do.
	t.Cleanup(func() { dialAndRequestShutdown(t, context.Background(), mustSocketPath(t, projectDir)) })

	var _ workspace.Workspace = client

	_, err = client.ListSessions(ctx)
	require.NoError(t, err, "unary call over the connected client should succeed")

	require.NoError(t, prefs.SetCompactMode(true))
	require.True(t, prefs.Prefs().CompactMode)

	// SetCompactMode's ScopeGlobal write lands in config.GlobalConfigData()
	// (SENNIT_GLOBAL_DATA/sennit.json) -- a different file from the one
	// writeGlobalConfig seeded (SENNIT_GLOBAL_CONFIG/sennit.json, one of
	// the other global layers config.Load merges in read-only here).
	data, err := os.ReadFile(config.GlobalConfigData())
	require.NoError(t, err)
	require.Contains(t, string(data), `"compact_mode":true`, "expected options.tui.compact_mode written to the client's own config file")
}

// TestSetupDaemonWorkspace_CleanupDoesNotStopDaemon pins the quitting
// contract from CLIENT-SERVER.md's PR 2.3: cleanup (what runInteractiveDaemon
// defers, and what a real quit runs) must only end this client's own pump
// and connection, never the daemon itself -- a busy session the daemon is
// still running must survive the TUI quitting on it. Introducing a
// RequestShutdown call into cleanup should turn this red; see this
// package's own daemon_client.go doc comments for the invariant.
func TestSetupDaemonWorkspace_CleanupDoesNotStopDaemon(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	command := helperCommand(t)

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()

	_, _, cleanup, err := setupDaemonWorkspace(ctx, projectDir, "", false, supervisor.Options{Command: command})
	require.NoError(t, err)
	socketPath := mustSocketPath(t, projectDir)

	cleanup()

	// The daemon must still be reachable: a fresh connection succeeds and
	// answers Hello, exactly as if the client had never quit.
	conn2, err := supervisor.Dial(socketPath)
	require.NoError(t, err)
	defer conn2.Close()
	t.Cleanup(func() { dialAndRequestShutdown(t, ctx, socketPath) })

	client2 := grpcws.NewClient(conn2)
	defer client2.Shutdown()
	hello, err := client2.Hello(ctx)
	require.NoError(t, err, "daemon must still be serving after the previous client's cleanup")
	require.NotZero(t, hello.ProtocolVersion)
}

// mustSocketPath resolves the socket a daemon for projectDir binds to,
// the same way EnsureRunning derives it -- used only for test teardown,
// independent of whatever *grpcws.Client the test under it is exercising.
func mustSocketPath(t *testing.T, projectDir string) string {
	t.Helper()
	socketPath, _, err := daemon.ResolveSocketPath(context.Background(), projectDir, "", false)
	require.NoError(t, err)
	return socketPath
}

// dialAndRequestShutdown tears down a daemon this file's tests started, by
// asking it to shut down unconditionally and waiting for its socket to
// stop answering -- supervisor.spawnDetached deliberately releases
// (rather than waits on) the child process it starts, so a real RPC is
// the only reliable, cross-platform way back to it.
func dialAndRequestShutdown(t *testing.T, ctx context.Context, socketPath string) {
	t.Helper()
	conn, err := supervisor.Dial(socketPath)
	if err != nil {
		return
	}
	defer conn.Close()
	client := grpcws.NewClient(conn)
	defer client.Shutdown()
	_, _ = client.RequestShutdown(ctx, false)
}

// TestSetupDaemonWorkspace_ErrTUILocked covers the lock-holder path: when
// the project's workspace lock is held by an embedded TUI (ModeTUI, not
// a daemon), setupDaemonWorkspace must surface the same *supervisor.
// ErrTUILocked an in-process caller sees as workspacelock.ErrLocked --
// same wording, no daemon-specific rewrap.
func TestSetupDaemonWorkspace_ErrTUILocked(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()

	_, lockDir, err := daemon.ResolveSocketPath(ctx, projectDir, "", false)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(lockDir, 0o700))

	helper := startLockHelper(t, lockDir)

	_, _, _, err = setupDaemonWorkspace(ctx, projectDir, "", false, supervisor.Options{Command: helperCommand(t)})
	require.Error(t, err)
	require.ErrorIs(t, err, workspacelock.ErrLocked)
	var lockErr *supervisor.ErrTUILocked
	require.ErrorAsf(t, err, &lockErr, "expected *supervisor.ErrTUILocked, got %T: %v", err, err)
	require.Equal(t, helper.pid, lockErr.PID)
	// The message must be exactly the wrapped cause -- no "--daemon"
	// hint appended, since this isn't a daemon-connect failure.
	require.NotContains(t, err.Error(), "--daemon")
}

// TestSetupDaemonWorkspace_ConnectFailureHintsNoDaemon covers any other
// EnsureRunning failure (here: a spawn hook that can never produce a
// healthy daemon before the readiness timeout): the returned error must
// be non-nil and must point at dropping --daemon as the way to recover.
func TestSetupDaemonWorkspace_ConnectFailureHintsNoDaemon(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	projectDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()

	failingCommand := func(args []string) *exec.Cmd {
		// "false" exits immediately without ever binding a socket, so
		// EnsureRunning's readiness wait always times out.
		return exec.CommandContext(context.Background(), "false") //nolint:gosec
	}

	_, _, _, err := setupDaemonWorkspace(ctx, projectDir, "", false, supervisor.Options{
		Command:      failingCommand,
		ReadyTimeout: 500 * time.Millisecond,
	})
	require.Error(t, err)
	require.NotErrorIs(t, err, workspacelock.ErrLocked)
	require.Contains(t, err.Error(), "--daemon")
}

// TestPrintDaemonQuitNote_BusySession covers the quit note itself, as
// plain logic over a workspace.AgentActivity value -- no daemon needed
// (see printDaemonQuitNote's own doc comment on why it takes this rather
// than a live client).
func TestPrintDaemonQuitNote_BusySession(t *testing.T) {
	var buf strings.Builder
	printDaemonQuitNote(&buf, workspace.AgentActivity{BusySessions: []string{"sess-1"}})
	require.Contains(t, buf.String(), "Work continues in the background")
	require.Contains(t, buf.String(), "sennit")
}

func TestPrintDaemonQuitNote_NoBusySessions(t *testing.T) {
	var buf strings.Builder
	printDaemonQuitNote(&buf, workspace.AgentActivity{})
	require.Empty(t, buf.String())
}
