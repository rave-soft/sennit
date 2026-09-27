package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/message"
)

// TestPermissionWaitsWithoutClients covers CLIENT-SERVER.md PR 2.4's
// second scenario together with the review's point 4 (a pending
// permission counts as busy, not just an active client/session): a tool
// call that needs permission is raised while nobody is connected, the
// daemon stays up past its own configured idle_timeout while it waits,
// and a client that connects afterward sees the pending request on
// Subscribe's snapshot replay, grants it, and the turn completes.
func TestPermissionWaitsWithoutClients(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("daemon e2e: skipped under -race, see racecheck_off_test.go")
	}

	fixture := newFixtureServer(
		fixtureTurn{ToolCalls: []fixtureToolCall{{ID: "call_1", Name: tools.BashToolName, Args: `{"description":"write a file","command":"printf 'hi' > out.txt"}`}}},
		fixtureTurn{Text: "done after permission"},
	)
	defer fixture.Close()

	projectDir := t.TempDir()
	// A short idle_timeout so this test can actually observe the daemon
	// staying up past it while nobody is connected and a permission is
	// pending, rather than merely asserting it never got the chance to
	// try. The idle monitor still only re-checks every
	// defaultIdlePollInterval (5s, not configurable from the CLI this
	// package drives), so the "outlives idle_timeout" wait below has to
	// be at least one poll tick, not just idle_timeout itself.
	writeShortIdleTimeoutConfig(t, projectDir)

	dp := startDaemonProcess(t, projectDir, fixture.URL)

	client, closeClient := dp.dial(t)
	ctx, cancel := context.WithTimeout(context.Background(), hangGuard)
	defer cancel()

	sess, err := client.CreateSession(ctx, "permission wait test")
	require.NoError(t, err)
	require.NoError(t, client.AgentRun(ctx, sess.ID, "run a command"))

	var lastPending int
	ok := assert.Eventually(t, func() bool {
		pending, err := client.PendingPrompts(ctx)
		if err == nil {
			lastPending = len(pending.Permissions)
		}
		return err == nil && len(pending.Permissions) > 0
	}, hangGuard, 50*time.Millisecond, "permission request never appeared")
	if !ok {
		out := dp.out.String()
		t.Logf("daemon output:\n%s", out)
		if _, after, found := strings.Cut(out, "Daemon log: "); found {
			logPath := strings.TrimSpace(strings.SplitN(after, "\n", 2)[0])
			if content, rerr := os.ReadFile(logPath); rerr == nil {
				t.Logf("daemon log file %s:\n%s", logPath, content)
			}
		}
		msgs, _ := client.ListMessages(ctx, sess.ID)
		for _, m := range msgs {
			t.Logf("message role=%s finish=%v text=%q", m.Role, m.FinishPart(), m.Content().Text)
		}
		t.Fatalf("permission request never appeared (last seen count %d)", lastPending)
	}

	closeClient() // nobody connected while the request is pending.

	// Outlive idle_timeout (1s) by comfortably more than one poll tick
	// (5s default): if the pending-permission check were ever dropped
	// from the busy condition (see CLIENT-SERVER-REVIEW.md point 4), this
	// is where the daemon would have exited already.
	time.Sleep(7 * time.Second)
	require.True(t, dp.stillRunning(t), "daemon exited while a permission request was still pending")

	client2, _ := dp.dial(t)
	pending, err := client2.PendingPrompts(ctx)
	require.NoError(t, err)
	require.Len(t, pending.Permissions, 1)
	req := pending.Permissions[0]
	require.Equal(t, sess.ID, req.SessionID)

	granted, err := client2.PermissionGrant(req)
	require.NoError(t, err)
	require.True(t, granted)

	require.Eventually(t, func() bool {
		return !client2.AgentIsSessionBusy(sess.ID)
	}, hangGuard, 50*time.Millisecond, "turn never finished after the permission was granted")

	msgs, err := client2.ListMessages(ctx, sess.ID)
	require.NoError(t, err)
	found := false
	for _, m := range msgs {
		if m.Role == message.Assistant && m.Content().Text == "done after permission" {
			found = true
		}
	}
	require.True(t, found, "expected the post-permission assistant reply among %d messages", len(msgs))

	shutdownCleanly(t, dp, client2)
}
