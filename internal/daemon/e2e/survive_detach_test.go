package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/message"
)

// TestSurviveDetach covers CLIENT-SERVER.md PR 2.4's first scenario: a
// turn dispatched by one client keeps running, and finishes correctly,
// after that client disconnects mid-turn -- workspace.AgentRun is
// fire-and-forget by contract (see its doc comment: "cancelling [ctx]
// does not stop an already-accepted run"), and this exercises that
// promise end to end, through the real daemon binary and a real dropped
// connection, not just appws's in-process unit test
// (TestAppWorkspace_Shutdown_JoinsRunDispatchedViaAgentRun).
func TestSurviveDetach(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("daemon e2e: skipped under -race, see racecheck_off_test.go")
	}

	const reply = "the slow turn finished after detach"
	fixture := newFixtureServer(fixtureTurn{Text: reply, ChunkDelay: 300 * time.Millisecond})
	defer fixture.Close()

	projectDir := t.TempDir()
	dp := startDaemonProcess(t, projectDir, fixture.URL)

	clientA, closeA := dp.dial(t)
	ctx, cancel := context.WithTimeout(context.Background(), hangGuard)
	defer cancel()

	sess, err := clientA.CreateSession(ctx, "detach test")
	require.NoError(t, err)

	require.NoError(t, clientA.AgentRun(ctx, sess.ID, "please answer slowly"))

	// Give the turn a moment to actually start streaming before pulling
	// the rug out from under it -- AgentRun returning only promises the
	// turn was accepted, not that a token has been emitted yet.
	require.Eventually(t, func() bool {
		return clientA.AgentIsSessionBusy(sess.ID)
	}, hangGuard, 20*time.Millisecond, "turn never became visibly busy before detach")

	closeA() // drop client A's connection entirely, mid-turn.

	clientB, _ := dp.dial(t)
	require.Eventually(t, func() bool {
		return !clientB.AgentIsSessionBusy(sess.ID)
	}, hangGuard, 50*time.Millisecond, "turn never finished after detach")

	msgs, err := clientB.ListMessages(ctx, sess.ID)
	require.NoError(t, err)

	found := false
	for _, m := range msgs {
		if m.Role == message.Assistant && m.Content().Text == reply {
			found = true
		}
	}
	require.True(t, found, "expected the complete assistant reply %q among %d messages", reply, len(msgs))

	shutdownCleanly(t, dp, clientB)
}
