package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/testenv"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/daemon"
	"github.com/rave-soft/sennit/internal/daemon/supervisor"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspacelock"
)

// daemonCmdTestCommand builds a fresh *cobra.Command carrying every flag
// this file's commands read, chdir'd into project (restored on cleanup --
// see trustTestCommand's own doc comment on why that matters on
// Windows), with its context set so cmd.Context() inside a RunE is never
// nil (cobra only sets that through Execute/ExecuteContext, neither of
// which these tests go through).
func daemonCmdTestCommand(t *testing.T, project string) *cobra.Command {
	t.Helper()
	before, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.Chdir(before)) })

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.Flags().String("cwd", project, "")
	cmd.Flags().String("data-dir", "", "")
	cmd.Flags().Bool("debug", false, "")
	cmd.Flags().Bool("trust-project", false, "")
	cmd.Flags().String("session", "", "")
	cmd.Flags().Bool("continue", false, "")
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().Bool("force", false, "")
	cmd.Flags().Bool("follow", false, "")
	cmd.Flags().Bool("detach", false, "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().Bool("quiet", false, "")
	cmd.Flags().Bool("verbose", false, "")
	return cmd
}

// startTestDaemon spawns a real daemon (via the TestCmdDaemonHelperProcess
// re-exec helper daemon_client_test.go already defines) for project,
// through supervisor.EnsureRunning directly -- spawning is fine as test
// setup even for commands (attach/ps/daemon status) that must themselves
// never spawn one. It returns the socket path and registers teardown.
func startTestDaemon(t *testing.T, ctx context.Context, project string) string {
	t.Helper()
	socketPath, _, err := supervisor.EnsureRunning(ctx, project, supervisor.Options{Command: helperCommand(t)})
	require.NoError(t, err)
	t.Cleanup(func() { dialAndRequestShutdown(t, context.Background(), project) })
	return socketPath
}

// TestAttachCmd_NoDaemonRunning covers attach's "never spawn" contract:
// with nothing running for the project, attach must fail with a clear
// message rather than starting a daemon.
func TestAttachCmd_NoDaemonRunning(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", testenv.ShortRuntimeDir(t))
	project := t.TempDir()

	_, _, _, err := setupAttachWorkspace(context.Background(), project, "", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no daemon running")
}

// TestAttachCmd_ConnectsToRunningDaemon covers the happy path: a daemon
// already running for the project is found (never spawned) and
// setupAttachWorkspace returns a connected client.
func TestAttachCmd_ConnectsToRunningDaemon(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", testenv.ShortRuntimeDir(t))
	project := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()

	startTestDaemon(t, ctx, project)

	client, prefs, cleanup, err := setupAttachWorkspace(ctx, project, "", false)
	require.NoError(t, err)
	defer cleanup()
	require.NotNil(t, prefs)

	_, err = client.ListSessions(ctx)
	require.NoError(t, err)
}

// TestPSCmd_NoDaemonRunning covers `sennit ps` against an absent daemon:
// exit 0 (nil error), plain message, never spawns.
func TestPSCmd_NoDaemonRunning(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", testenv.ShortRuntimeDir(t))
	project := t.TempDir()

	cmd := daemonCmdTestCommand(t, project)
	require.NoError(t, psCmd.RunE(cmd, nil))
}

// TestPSCmd_RunningDaemonIdle covers `sennit ps` against a real, idle
// daemon: no error, and (with --json) an empty-but-well-formed report.
func TestPSCmd_RunningDaemonIdle(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", testenv.ShortRuntimeDir(t))
	project := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()
	startTestDaemon(t, ctx, project)

	cmd := daemonCmdTestCommand(t, project)
	cmd.SetContext(ctx)
	require.NoError(t, cmd.Flags().Set("json", "true"))
	var out strings.Builder
	cmd.SetOut(&out)
	require.NoError(t, psCmd.RunE(cmd, nil))

	var report psReport
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	require.Empty(t, report.BusySessions)
	require.Empty(t, report.Permissions)
}

// fakePSSource is a hand-written psSource fake -- narrower than
// wsrpctest.StubWorkspace and doesn't need it, since collectPS only
// reads the handful of methods psSource declares. It seeds a busy
// session and a pending permission request the way a real daemon mid-
// turn would report them, without driving one.
type fakePSSource struct {
	activity   workspace.AgentActivity
	prompts    workspace.PendingPrompts
	sessions   []session.Session
	threads    []proto.Thread
	tasks      []proto.Thread
	hasThreads bool
	hasTasks   bool
	listErr    error
	promptsErr error
	threadsErr error
	tasksErr   error
}

func (f *fakePSSource) AgentActivity() workspace.AgentActivity { return f.activity }

func (f *fakePSSource) PendingPrompts(context.Context) (workspace.PendingPrompts, error) {
	return f.prompts, f.promptsErr
}

func (f *fakePSSource) ListSessions(context.Context) ([]session.Session, error) {
	return f.sessions, f.listErr
}

func (f *fakePSSource) SupportsThreads() bool { return f.hasThreads }

func (f *fakePSSource) ListThreads(context.Context) ([]proto.Thread, error) {
	return f.threads, f.threadsErr
}

func (f *fakePSSource) SupportsTasks() bool { return f.hasTasks }

func (f *fakePSSource) ListTasks(context.Context) ([]proto.Thread, error) {
	return f.tasks, f.tasksErr
}

// TestCollectPS_BusySessionAndPendingPermission is the seeded-fixture
// test the task asks for: a busy session (with a queued follow-up) and a
// pending permission request must both show up in collectPS's report.
func TestCollectPS_BusySessionAndPendingPermission(t *testing.T) {
	src := &fakePSSource{
		activity: workspace.AgentActivity{
			BusySessions:  []string{"sess-1"},
			QueuedPrompts: map[string][]string{"sess-1": {"follow up"}},
		},
		sessions: []session.Session{{ID: "sess-1", Title: "Fixing the bug"}},
		prompts: workspace.PendingPrompts{
			Permissions: []permission.PermissionRequest{
				{ID: "perm-1", SessionID: "sess-1", ToolName: "bash", Description: "run tests"},
			},
		},
	}

	report, err := collectPS(context.Background(), src)
	require.NoError(t, err)
	require.Len(t, report.BusySessions, 1)
	require.Equal(t, "sess-1", report.BusySessions[0].ID)
	require.Equal(t, "Fixing the bug", report.BusySessions[0].Title)
	require.Equal(t, 1, report.BusySessions[0].QueuedPrompts)

	require.Len(t, report.Permissions, 1)
	require.Equal(t, "bash", report.Permissions[0].ToolName)
	require.Equal(t, "run tests", report.Permissions[0].Description)

	var buf strings.Builder
	printPS(&buf, report)
	require.Contains(t, buf.String(), "Fixing the bug")
	require.Contains(t, buf.String(), "bash")
	require.Contains(t, buf.String(), "run tests")
}

// TestCollectPS_Idle covers the empty case: nothing busy, nothing
// pending, printPS says so in one line.
func TestCollectPS_Idle(t *testing.T) {
	report, err := collectPS(context.Background(), &fakePSSource{})
	require.NoError(t, err)
	require.Empty(t, report.BusySessions)
	require.Empty(t, report.Permissions)
	require.Empty(t, report.Questions)

	var buf strings.Builder
	printPS(&buf, report)
	require.Contains(t, buf.String(), "idle")
}

// TestDaemonStatusCmd covers `daemon status` both ways: not running
// (exit 0, Running: false) and running (PID matches the lock's own
// owner, version/protocol populated from a live Hello).
func TestDaemonStatusCmd(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", testenv.ShortRuntimeDir(t))
	project := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()

	notRunning, err := daemonStatus(ctx, project, "", false)
	require.NoError(t, err)
	require.False(t, notRunning.Running)

	startTestDaemon(t, ctx, project)

	_, lockDir, err := daemon.ResolveSocketPath(ctx, project, "", false)
	require.NoError(t, err)
	owner, ok, err := workspacelock.CurrentOwner(lockDir)
	require.NoError(t, err)
	require.True(t, ok)

	running, err := daemonStatus(ctx, project, "", false)
	require.NoError(t, err)
	require.True(t, running.Running)
	require.Equal(t, owner.PID, running.PID)
	require.NotZero(t, running.ProtocolVersion)
	require.NotEmpty(t, running.LogPath)
}

// TestDaemonStopCmd covers stop's busy/idle split: an idle daemon stops
// and releases its socket; a busy one (simulated with AppReady raising a
// permission request through the App directly, the same seam idle_test.go
// uses) is refused without --force and accepted with it.
func TestDaemonStopCmd_Idle(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", testenv.ShortRuntimeDir(t))
	project := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()

	socketPath := startTestDaemon(t, ctx, project)

	// supervisor.EnsureRunning's own version check (checkVersion) already
	// made -- and finished -- an RPC against this daemon; a disconnected
	// client must not count as busy just because it's still within its
	// handle-release grace period (grpcws's leaseManager, fixed in PR
	// 2.3b round 1), so this must accept on the very first try.
	accepted, _, err := requestDaemonShutdown(ctx, project, "", false, socketPath, false)
	require.NoError(t, err)
	require.True(t, accepted)

	require.NoError(t, supervisor.AwaitGone(ctx, socketPath))
	require.False(t, supervisor.ProbeHealthy(ctx, socketPath, raceWait(500*time.Millisecond)))
}

// startInProcessDaemon runs daemon.Run directly, in-process, in a
// goroutine, rather than through the subprocess-spawning
// supervisor/helperCommand path this file's other tests use: reaching
// into a spawned subprocess's own *app.App to raise a permission request
// by hand isn't possible, so a genuinely busy daemon (as opposed to one
// with an open connection) has to be driven the same way
// internal/daemon/idle_test.go does it, via Options.AppReady. Returns the
// socket path once Run reports ready and registers a forced shutdown at
// test end.
func startInProcessDaemon(t *testing.T, ctx context.Context, project string, appReady func(*app.App)) string {
	t.Helper()
	ready := make(chan string, 1)
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- daemon.Run(ctx, project, daemon.Options{
			Ready:    func(s string) { ready <- s },
			LogSetup: func(string, bool) {},
			AppReady: appReady,
		})
	}()
	var socketPath string
	select {
	case socketPath = <-ready:
	case err := <-runErrCh:
		t.Fatalf("daemon exited before becoming ready: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for the daemon to become ready")
	}
	// This daemon runs in-process (the goroutine above), not as a spawned
	// subprocess, so there is no separate PID to wait for the way
	// dialAndRequestShutdown does for the rest of this file's tests: this
	// test process IS the daemon here. What must actually finish before
	// this cleanup returns is daemon.Run's own goroutine -- it holds
	// sennit.db open until it does -- so wait on runErrCh, not just the
	// socket going quiet.
	t.Cleanup(func() {
		conn, err := supervisor.Dial(socketPath)
		if err == nil {
			client := grpcws.NewClient(conn)
			_, _ = client.RequestShutdown(context.Background(), false)
			client.Shutdown()
			conn.Close()
		}
		select {
		case <-runErrCh:
		case <-time.After(daemonTestTimeout):
			t.Fatal("in-process daemon.Run did not return after shutdown")
		}
	})
	return socketPath
}

// TestDaemonStopCmd_Busy is the busy half of TestDaemonStopCmd_Idle: a
// daemon with a genuinely pending permission request (the same seam
// internal/daemon/idle_test.go uses to drive busyness without a real
// agent turn) refuses a plain `daemon stop`, names the tool that's
// waiting in its output, and returns a non-zero (error) result; `--force`
// stops it anyway.
func TestDaemonStopCmd_Busy(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", testenv.ShortRuntimeDir(t))
	project := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()

	var perms permission.Service
	socketPath := startInProcessDaemon(t, ctx, project, func(a *app.App) {
		perms = a.Permissions()
	})

	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		_, _ = perms.Request(ctx, permission.CreatePermissionRequest{
			SessionID:  "sess-1",
			ToolCallID: "call-1",
			ToolName:   "bash",
			Action:     "run",
			Path:       project,
		})
	}()
	t.Cleanup(func() { <-requestDone })
	require.Eventually(t, func() bool {
		_, ok := perms.ActiveRequest()
		return ok
	}, daemonTestTimeout, 5*time.Millisecond, "expected the permission request to become active")

	cmd := daemonCmdTestCommand(t, project)
	cmd.SetContext(ctx)
	var out strings.Builder
	cmd.SetOut(&out)
	err := daemonStopCmd.RunE(cmd, nil)
	require.Error(t, err, "a busy daemon must refuse a plain stop")
	require.Contains(t, err.Error(), "busy")
	require.Contains(t, out.String(), "bash", "expected the busy report to name the waiting tool")

	// The refusal must not have touched the daemon: still healthy.
	require.True(t, supervisor.ProbeHealthy(ctx, socketPath, raceWait(time.Second)))

	forceCmd := daemonCmdTestCommand(t, project)
	forceCmd.SetContext(ctx)
	require.NoError(t, forceCmd.Flags().Set("force", "true"))
	require.NoError(t, daemonStopCmd.RunE(forceCmd, nil), "--force must stop a busy daemon anyway")
	require.NoError(t, supervisor.AwaitGone(ctx, socketPath))
}

// TestDaemonRestartCmd covers restart: the daemon that comes back has a
// different PID than the one that was stopped.
func TestDaemonRestartCmd(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", testenv.ShortRuntimeDir(t))
	project := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()

	startTestDaemon(t, ctx, project)
	_, lockDir, err := daemon.ResolveSocketPath(ctx, project, "", false)
	require.NoError(t, err)
	before, ok, err := workspacelock.CurrentOwner(lockDir)
	require.NoError(t, err)
	require.True(t, ok)

	// This RunE re-execs the test binary itself (daemonRestartCmd's
	// production spawn path, no Command hook to intercept), which under
	// -race means running a chunk of this package's own suite before it
	// reaches the daemon-helper test that answers health checks (see
	// daemon_client_test.go's TestCmdDaemonHelperProcess and
	// helperCommand's doc comment) -- comfortably fast without -race,
	// but slow enough under it to need more than supervisor's default
	// 10s readiness budget. Widen it here rather than in
	// defaultReadyTimeout itself, which stays the real product default.
	daemonRestartReadyTimeout = raceWait(10 * time.Second)
	t.Cleanup(func() { daemonRestartReadyTimeout = 0 })

	cmd := daemonCmdTestCommand(t, project)
	cmd.SetContext(ctx)
	require.NoError(t, daemonRestartCmd.RunE(cmd, nil))
	t.Cleanup(func() { dialAndRequestShutdown(t, context.Background(), project) })

	after, ok, err := workspacelock.CurrentOwner(lockDir)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotEqual(t, before.PID, after.PID)
}

// TestCmdDaemonLoggingHelperProcess is a subprocess helper (gated by
// SENNIT_CMD_DAEMON_LOGGING_HELPER=1), like TestCmdDaemonHelperProcess
// but with a LogSetup that actually writes a marker line to the log file
// instead of the no-op every other test here uses -- TestDaemonLogsCmd
// is the one test that needs there to genuinely be something on disk for
// `daemon logs` to show.
func TestCmdDaemonLoggingHelperProcess(t *testing.T) {
	if os.Getenv("SENNIT_CMD_DAEMON_LOGGING_HELPER") != "1" {
		return
	}
	cwd := os.Args[len(os.Args)-1]
	if err := daemon.Run(context.Background(), cwd, daemon.Options{
		LogSetup: func(logFile string, _ bool) {
			_ = os.MkdirAll(filepath.Dir(logFile), 0o700)
			_ = os.WriteFile(logFile, []byte("sennit daemon test log marker\n"), 0o600)
		},
	}); err != nil {
		// See TestCmdDaemonHelperProcess: don't let a real failure here
		// disappear silently.
		fmt.Fprintln(os.Stderr, "daemon.Run failed:", err)
	}
}

func loggingHelperCommand(t *testing.T) func(args []string) *exec.Cmd {
	t.Helper()
	t.Setenv("SENNIT_CMD_DAEMON_LOGGING_HELPER", "1")
	// See main_test.go's TestMain: this test needs the subprocess to
	// agree with this process on where the global profile (and so
	// GlobalLogDir) lives, not get its own independent one.
	t.Setenv("SENNIT_CMD_REUSE_GLOBAL_PROFILE", "1")
	return func(args []string) *exec.Cmd {
		cwd := extractCwdFlag(args)
		return exec.CommandContext(context.Background(), os.Args[0], "-test.run=TestCmdDaemonLoggingHelperProcess", "--", cwd) //nolint:gosec
	}
}

// TestDaemonLogsCmd covers `daemon logs`: the daemon's own process log
// file (found from the workspace lock's recorded PID) contains the
// marker line TestCmdDaemonLoggingHelperProcess wrote.
func TestDaemonLogsCmd(t *testing.T) {
	writeGlobalConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", testenv.ShortRuntimeDir(t))
	project := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), daemonTestTimeout)
	defer cancel()

	_, _, err := supervisor.EnsureRunning(ctx, project, supervisor.Options{Command: loggingHelperCommand(t)})
	require.NoError(t, err)
	t.Cleanup(func() { dialAndRequestShutdown(t, context.Background(), project) })

	_, lockDir, err := daemon.ResolveSocketPath(ctx, project, "", false)
	require.NoError(t, err)
	owner, ok, err := workspacelock.CurrentOwner(lockDir)
	require.NoError(t, err)
	require.True(t, ok)

	logPath := daemonLogPath(owner.PID)
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(logPath)
		return err == nil && len(data) > 0
	}, raceWait(10*time.Second), 100*time.Millisecond, "expected the daemon's log file to appear")

	cmd := daemonCmdTestCommand(t, project)
	cmd.SetContext(ctx)
	var out strings.Builder
	cmd.SetOut(&out)
	require.NoError(t, daemonLogsCmd.RunE(cmd, nil))
	require.Contains(t, out.String(), filepath.Base(logPath))
	require.Contains(t, out.String(), "sennit daemon test log marker")
}
