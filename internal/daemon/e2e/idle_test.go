package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestIdleExitsWithNothingRunning is CLIENT-SERVER.md PR 2.4's fifth
// scenario, the "nothing running" half: with no clients connected and no
// busy session, the daemon exits on its own after idle_timeout, removing
// its socket and releasing the workspace lock -- internal/daemon's own
// idle_test.go covers the busy-condition logic in depth and fast
// (synthetic poll interval); this is the one place that exercises the
// real binary actually doing it, so it has to live with the production
// idle-poll interval (5s, not configurable from the CLI) rather than a
// synthetic one.
func TestIdleExitsWithNothingRunning(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("daemon e2e: skipped under -race, see racecheck_off_test.go")
	}

	fixture := newFixtureServer(fixtureTurn{Text: "hello"})
	defer fixture.Close()

	projectDir := t.TempDir()
	writeShortIdleTimeoutConfig(t, projectDir)

	dp := startDaemonProcess(t, projectDir, fixture.URL)
	// No client ever connects: startDaemonProcess's own readiness probe
	// dials and closes immediately (supervisor.ProbeHealthy), so there is
	// nothing left open behind it.

	err := dp.waitExit(t)
	require.NoError(t, err, "expected the idle daemon to exit cleanly on its own")
	dp.requireGone(t)
}

// TestIdleDoesNotExitDuringATurn is the other half: a busy session must
// keep the daemon up through the same idle_timeout that would otherwise
// have retired it, and only the idle clock starting fresh once the turn
// actually finishes governs when it exits.
func TestIdleDoesNotExitDuringATurn(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("daemon e2e: skipped under -race, see racecheck_off_test.go")
	}

	fixture := newFixtureServer(fixtureTurn{Text: "a slow answer", ChunkDelay: 800 * time.Millisecond})
	defer fixture.Close()

	projectDir := t.TempDir()
	writeShortIdleTimeoutConfig(t, projectDir)

	dp := startDaemonProcess(t, projectDir, fixture.URL)
	client, closeClient := dp.dial(t)
	ctx, cancel := context.WithTimeout(context.Background(), hangGuard)
	defer cancel()

	sess, err := client.CreateSession(ctx, "idle during turn test")
	require.NoError(t, err)
	require.NoError(t, client.AgentRun(ctx, sess.ID, "answer slowly"))
	require.Eventually(t, func() bool {
		return client.AgentIsSessionBusy(sess.ID)
	}, hangGuard, 20*time.Millisecond, "turn never became visibly busy")
	closeClient()

	// The turn itself (a few words at 800ms apiece) is comfortably
	// shorter than one idle poll tick (5s); if idle_timeout alone
	// governed exit regardless of busyness, the daemon would already be
	// gone by the time the turn finishes.
	select {
	case err := <-dp.waitCh:
		dp.waitCh <- err
		t.Fatalf("daemon exited while a turn was still in flight (err=%v)", err)
	case <-time.After(2 * time.Second):
	}

	err = dp.waitExit(t)
	require.NoError(t, err, "expected the daemon to exit once idle after the turn finished")
	dp.requireGone(t)
}
