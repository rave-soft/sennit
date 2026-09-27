package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/testenv"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/daemon"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/question"
)

// idlePollInterval and idleObserveWindow are the short durations these
// tests use in place of options.daemon.idle_timeout's real 10m default, so
// a test observes idle exit (or its absence) in well under a second
// rather than waiting out production timescales. idleTimeoutConfig is the
// value written into each test's sennit.json.
//
// idlePollInterval and idleTimeout are widened under -race (see
// racecheck_off_test.go): they are real scheduling deadlines the daemon's
// idle monitor goroutine has to hit, not just hang-guard budgets, so
// -race's instrumentation overhead and CI's cross-package CPU contention
// can otherwise make a monitor tick land late enough to flake these tests
// even though nothing about idle detection itself is broken. Widening
// both by the same factor keeps every relationship the doc comments below
// rely on (idleObserveWindow vs. idleTimeout, poll vs. timeout) intact,
// just slower.
var (
	idlePollInterval  = raceIdleScale(15 * time.Millisecond)
	idleTimeout       = raceIdleScale(80 * time.Millisecond)
	idleTimeoutConfig = idleTimeout.String()
	// idleObserveWindow bounds how long a "must NOT have exited yet" check
	// waits before concluding the daemon really is staying up. This has to
	// be comfortably longer than idleTimeout plus a few poll ticks, not
	// just longer than idleTimeout itself: a busyness check that silently
	// stopped checking one source (say, connected clients) still takes
	// idleTimeout from the monitor's first idle observation to actually
	// exit, so a short window can pass by sheer luck even when the source
	// it's meant to hold busy has gone missing. Reproduced by hand while
	// writing this test: deleting the ClientCount check let a client-
	// connected daemon exit within about 300ms-2s of real time, which a
	// 5x-idleTimeout (400ms) window sometimes missed and sometimes caught.
	idleObserveWindow = 25 * idleTimeout
)

// raceIdleScale widens a daemon idle-timing value under -race, for the
// same reason raceWait widens hang-guard budgets elsewhere (see
// grpcws_test_helpers_test.go's doc comment) -- except these are the
// system under test's own configured deadlines, not just a test's wait
// budget, so every caller scales together rather than each picking its
// own multiplier.
func raceIdleScale(d time.Duration) time.Duration {
	if raceDetectorEnabled {
		return d * 6
	}
	return d
}

// writeDaemonIdleConfig writes a project-scoped sennit.json setting
// options.daemon.idle_timeout, mirroring how a real project would opt into
// a non-default value.
func writeDaemonIdleConfig(t *testing.T, projectDir, idleTimeout string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "sennit.json"),
		[]byte(`{"options": {"daemon": {"idle_timeout": "`+idleTimeout+`"}}}`), 0o644))
}

// startIdleDaemon is startDaemon plus the short poll interval every test
// in this file needs; appReady lets a test reach into the bootstrapped
// App directly (raising a permission/question request, swapping in a
// busy coordinator) since workspace.Workspace itself exposes no way to.
func startIdleDaemon(t *testing.T, ctx context.Context, projectDir string, appReady func(*app.App)) (socketPath string, runErr <-chan error) {
	t.Helper()
	ready := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- daemon.Run(ctx, projectDir, daemon.Options{
			Ready:            func(s string) { ready <- s },
			LogSetup:         func(string, bool) {},
			IdlePollInterval: idlePollInterval,
			AppReady:         appReady,
			// The idle_timeout override lives in a project-scoped
			// sennit.json (writeDaemonIdleConfig); local project config
			// is only honored for a trusted project (see
			// TestBootstrap_ProjectRuntimeActivationRequiresTrust).
			TrustProject: true,
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

// requireStillRunning asserts runErr has not fired within idleObserveWindow.
func requireStillRunning(t *testing.T, runErr <-chan error) {
	t.Helper()
	select {
	case err := <-runErr:
		t.Fatalf("expected the daemon to still be running, but it exited (err=%v)", err)
	case <-time.After(idleObserveWindow):
	}
}

// requireExitsWithin asserts runErr fires (with no error) within
// testTimeout.
func requireExitsWithin(t *testing.T, runErr <-chan error) {
	t.Helper()
	select {
	case err := <-runErr:
		require.NoError(t, err)
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the daemon to exit on idle")
	}
}

// TestDaemon_IdleExitsAfterTimeout covers the base case (CLIENT-SERVER.md,
// PR 2.1/2.4): no clients, nothing running -- the daemon exits on its own
// once idle_timeout has elapsed, and cleans up its socket exactly as a
// signaled shutdown would.
func TestDaemon_IdleExitsAfterTimeout(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)

	projectDir := t.TempDir()
	writeDaemonIdleConfig(t, projectDir, idleTimeoutConfig)

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	socketPath, runErr := startIdleDaemon(t, ctx, projectDir, nil)
	requireExitsWithin(t, runErr)

	_, statErr := os.Stat(socketPath)
	require.True(t, os.IsNotExist(statErr), "expected the socket file to be removed after idle exit")
}

// TestDaemon_IdleTimeoutDisabled covers idle_timeout <= 0
// (config.DaemonOptions.EffectiveIdleTimeout's documented "never exit on
// idle" semantics): the daemon must NOT exit on its own no matter how long
// it sits idle. The test only waits out a bounded window and then shuts
// the daemon down itself (ctx cancel), rather than proving a negative
// forever.
func TestDaemon_IdleTimeoutDisabled(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)

	projectDir := t.TempDir()
	writeDaemonIdleConfig(t, projectDir, "0")

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	_, runErr := startIdleDaemon(t, ctx, projectDir, nil)
	requireStillRunning(t, runErr)

	cancel()
	awaitShutdown(t, runErr)
}

// TestDaemon_ClientKeepsAliveThenExits covers the
// "нет клиентов" half of the idle condition end to end: a client with an
// open Subscribe stream (opened by Connect, same as dialDaemon elsewhere in
// this package) holds the daemon up past what would otherwise be an idle
// exit, and disconnecting starts the idle clock.
func TestDaemon_ClientKeepsAliveThenExits(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)

	projectDir := t.TempDir()
	writeDaemonIdleConfig(t, projectDir, idleTimeoutConfig)

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	socketPath, runErr := startIdleDaemon(t, ctx, projectDir, nil)
	_, closeClient := dialDaemon(t, socketPath)

	requireStillRunning(t, runErr)

	closeClient()
	requireExitsWithin(t, runErr)
}

// TestDaemon_PendingPermissionKeepsAlive covers the owner decision
// recorded in CLIENT-SERVER.md ("разрешение без клиентов") and review
// point 4: a pending permission request holds the daemon open even with
// no clients connected at all. Granting it lets the idle clock start.
func TestDaemon_PendingPermissionKeepsAlive(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)

	projectDir := t.TempDir()
	writeDaemonIdleConfig(t, projectDir, idleTimeoutConfig)

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	var perms permission.Service
	_, runErr := startIdleDaemon(t, ctx, projectDir, func(a *app.App) {
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
			Path:       projectDir,
		})
	}()

	require.Eventually(t, func() bool {
		_, ok := perms.ActiveRequest()
		return ok
	}, testTimeout, 5*time.Millisecond, "expected the permission request to become active")

	requireStillRunning(t, runErr)

	active, ok := perms.ActiveRequest()
	require.True(t, ok)
	require.True(t, perms.Grant(active))
	<-requestDone

	requireExitsWithin(t, runErr)
}

// TestDaemon_PendingQuestionKeepsAlive is
// TestDaemon_PendingPermissionKeepsAlive's counterpart for
// question.Request (review point 4: a pending question must hold the
// daemon open exactly as a pending permission does, not just permissions).
func TestDaemon_PendingQuestionKeepsAlive(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)

	projectDir := t.TempDir()
	writeDaemonIdleConfig(t, projectDir, idleTimeoutConfig)

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	var questions question.Service
	_, runErr := startIdleDaemon(t, ctx, projectDir, func(a *app.App) {
		questions = a.Questions
	})

	req := question.Request{
		ID:         "q-1",
		SessionID:  "sess-1",
		ToolCallID: "call-1",
		Questions: []question.Question{
			{ID: "q1", Type: question.TypeYesNo, Text: "continue?", Description: "test question"},
		},
	}

	askDone := make(chan struct{})
	go func() {
		defer close(askDone)
		_, _ = questions.Ask(ctx, req)
	}()

	require.Eventually(t, func() bool {
		_, ok := questions.ActiveRequest()
		return ok
	}, testTimeout, 5*time.Millisecond, "expected the question request to become active")

	requireStillRunning(t, runErr)

	yes := true
	require.True(t, questions.Answer(req.ID, []question.Answer{{QuestionID: "q1", Yes: &yes}}))
	<-askDone

	requireExitsWithin(t, runErr)
}

// TestDaemon_BusySessionKeepsAlive covers AgentActivity.BusySessions
// holding the daemon open. Driving a real turn through the mock provider
// within this package's bounded test timeouts would be slow and timing-
// sensitive (see MEMORY's wall-clock-budgets note); instead this installs
// a coordinator decorator that wraps the App's REAL coordinator --
// delegating every other method to it (CancelAll, IsBusy, Steer, ... are
// all still exercised by App.Shutdown and must not nil-panic there) -- and
// only overrides BusySessions/IsSessionBusy/IsBusy to report one session
// busy.
func TestDaemon_BusySessionKeepsAlive(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	writeGlobalConfig(t)

	projectDir := t.TempDir()
	t.Cleanup(func() { testenv.AssertRemovableOnWindows(t, projectDir) })
	writeDaemonIdleConfig(t, projectDir, idleTimeoutConfig)

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	_, runErr := startIdleDaemon(t, ctx, projectDir, func(a *app.App) {
		real := a.Coordinator()
		require.NotNil(t, real, "expected the mock-provider config to produce a real coordinator")
		a.SetAgentCoordinatorForTest(&busySessionCoordinator{Coordinator: real, sessionID: "busy-sess"})
	})

	requireStillRunning(t, runErr)

	cancel()
	awaitShutdown(t, runErr)
}

// busySessionCoordinator decorates a real agent.Coordinator, reporting one
// extra always-busy session while forwarding everything else unchanged --
// until CancelAll runs. App.Shutdown's own teardown (internal/app/shutdown.go)
// calls coord.CancelAll() and then treats a coordinator that still reports
// IsBusy() as work that "did not stop before the shutdown deadline,"
// deliberately retaining the workspace lock and database rather than
// release them out from under supposedly-live work. A fake busy session
// that stayed busy forever (as this type did before this fix) trips that
// same retention path on the test's own cancel()+awaitShutdown at the end,
// leaking an open handle on projectDir's sennit.lock/sennit.db that
// t.TempDir()'s cleanup can delete on Linux but Windows refuses (mandatory
// locking) -- exactly the failure testenv.AssertRemovableOnWindows now
// catches here even on Linux. A real busy session stops being busy once
// canceled; this fake one must too.
type busySessionCoordinator struct {
	agent.Coordinator
	sessionID string
	cancelled atomic.Bool
}

func (c *busySessionCoordinator) CancelAll() {
	c.cancelled.Store(true)
	c.Coordinator.CancelAll()
}

func (c *busySessionCoordinator) BusySessions() []string {
	if c.cancelled.Load() {
		return c.Coordinator.BusySessions()
	}
	return append([]string{c.sessionID}, c.Coordinator.BusySessions()...)
}

func (c *busySessionCoordinator) IsSessionBusy(sessionID string) bool {
	if !c.cancelled.Load() && sessionID == c.sessionID {
		return true
	}
	return c.Coordinator.IsSessionBusy(sessionID)
}

func (c *busySessionCoordinator) IsBusy() bool {
	return !c.cancelled.Load() || c.Coordinator.IsBusy()
}

var _ agent.Coordinator = (*busySessionCoordinator)(nil)
