package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/message"
)

// TestKillRestartFinalizesInterruptedTurn covers CLIENT-SERVER.md PR
// 2.4's fourth scenario: a daemon killed with no chance to run its own
// shutdown path (kill -9, not SIGTERM) leaves an assistant message with
// no Finish behind; the next `daemon run` for the same project has to
// notice and mark that turn interrupted/cancelled on startup
// (internal/app/interrupted.go's finalizeInterruptedTurns runs as part of
// app.Bootstrap, before this daemon's socket ever exists) rather than
// leaving it looking perpetually in-progress, and a fresh turn afterward
// has to work normally.
func TestKillRestartFinalizesInterruptedTurn(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("daemon e2e: skipped under -race, see racecheck_off_test.go")
	}

	fixture := newFixtureServer(
		fixtureTurn{Text: "this reply never finishes streaming", ChunkDelay: 2 * time.Second},
		fixtureTurn{Text: "restarted reply"},
	)
	defer fixture.Close()

	projectDir := t.TempDir()
	setupGlobalProfile(t, fixture.URL)
	dp := startDaemonProcessNamed(t, projectDir)

	client, closeClient := dp.dial(t)
	ctx, cancel := context.WithTimeout(context.Background(), hangGuard)
	defer cancel()

	sess, err := client.CreateSession(ctx, "kill restart test")
	require.NoError(t, err)
	require.NoError(t, client.AgentRun(ctx, sess.ID, "start a slow reply"))

	require.Eventually(t, func() bool {
		if !client.AgentIsSessionBusy(sess.ID) {
			return false
		}
		msgs, err := client.ListMessages(ctx, sess.ID)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if m.Role == message.Assistant && m.FinishPart() == nil {
				return true
			}
		}
		return false
	}, hangGuard, 20*time.Millisecond, "the in-flight turn's own (unfinished) assistant message never appeared before the kill")

	closeClient()
	dp.killNow(t)
	err = dp.waitExit(t)
	require.Error(t, err, "expected a kill -9 to be reported as a non-zero exit, not a clean one")

	// The fixture server keeps its turn-index state, so restarting the
	// daemon against the same project and the same fixture serves the
	// second scripted turn ("a fresh turn afterward works") without
	// needing a second fixture.
	dp2 := startDaemonProcessNamed(t, projectDir)
	client2, _ := dp2.dial(t)

	msgs, err := client2.ListMessages(ctx, sess.ID)
	require.NoError(t, err)

	var assistant *message.Message
	for i := range msgs {
		if msgs[i].Role == message.Assistant {
			assistant = &msgs[i]
		}
	}
	require.NotNil(t, assistant, "expected the interrupted turn's assistant message to still be there")
	finish := assistant.FinishPart()
	require.NotNil(t, finish, "expected finalizeInterruptedTurns to have set a Finish on the cut-short turn")
	require.Equal(t, message.FinishReasonCanceled, finish.Reason)

	require.False(t, client2.AgentIsSessionBusy(sess.ID), "the interrupted turn must not still read as busy")

	// A fresh turn on the same session works normally after the restart.
	require.NoError(t, client2.AgentRun(ctx, sess.ID, "try again"))
	require.Eventually(t, func() bool {
		return !client2.AgentIsSessionBusy(sess.ID)
	}, hangGuard, 50*time.Millisecond, "the post-restart turn never finished")

	shutdownCleanly(t, dp2, client2)
}
