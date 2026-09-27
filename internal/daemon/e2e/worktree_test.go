package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/message"
)

// TestWorktreeThroughDaemon covers CLIENT-SERVER.md PR 2.4's worktree
// scenario: entering and exiting a worktree over the daemon (real
// EnterWorktree/ExitWorktree RPCs, a real git repo, a real second worktree
// directory on disk), and that the transfer is really exclusive -- a
// second, independently dialed connection cannot also claim the same
// session while the first holds it.
//
// The gap this test used to document -- a client that enters a worktree
// and then drops its connection entirely, rather than calling
// ExitWorktree, leaving no way for a fresh connection to resume it -- is
// closed by ResumeWorktree (CLIENT-SERVER.md, PR 2.4b); see
// TestWorktreeSurvivesDisconnectAndResumesOnReconnect below for that
// scenario end to end.
func TestWorktreeThroughDaemon(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("daemon e2e: skipped under -race, see racecheck_off_test.go")
	}

	fixture := newFixtureServer(fixtureTurn{Text: "hello"})
	defer fixture.Close()

	projectDir := t.TempDir()
	gitInitRepo(t, projectDir)

	dp := startDaemonProcess(t, projectDir, fixture.URL)
	ctx, cancel := context.WithTimeout(context.Background(), hangGuard)
	defer cancel()

	client1, _ := dp.dial(t)
	sess, err := client1.CreateSession(ctx, "worktree test")
	require.NoError(t, err)
	require.NoError(t, client1.SetCurrentSession(ctx, sess.ID))

	rootDir := client1.WorkingDir()

	handle, release, err := client1.EnterWorktree(ctx, "feature")
	require.NoError(t, err)

	require.NotEqual(t, rootDir, handle.WorkingDir(), "expected the worktree handle to work in its own directory")
	require.True(t, handle.WorktreeState().Active)
	require.Equal(t, "feature", handle.WorktreeState().Name)

	// A second, independently dialed connection must not be able to
	// claim the same session's worktree while client1 still holds it.
	client2, _ := dp.dial(t)
	require.NoError(t, client2.SetCurrentSession(ctx, sess.ID))
	_, _, err = client2.EnterWorktree(ctx, "another")
	require.Error(t, err, "expected entering a worktree for a session already transferred elsewhere to fail")
	t.Logf("second EnterWorktree correctly refused: %v", err)

	// Exit through the handle client1 still holds, restoring root
	// ownership of the session.
	root, exitRelease, err := handle.ExitWorktree(ctx)
	require.NoError(t, err)
	release() // the worktree handle itself; ExitWorktree registers a new handle for root but does not retire this one.
	require.False(t, root.WorktreeState().Active)
	require.Equal(t, rootDir, root.WorkingDir())

	// Now that the session is back at the root, a fresh connection can
	// transfer it again.
	client3, _ := dp.dial(t)
	require.NoError(t, client3.SetCurrentSession(ctx, sess.ID))
	handle2, release2, err := client3.EnterWorktree(ctx, "feature-2")
	require.NoError(t, err)
	require.True(t, handle2.WorktreeState().Active)

	release2()
	exitRelease()
	shutdownCleanly(t, dp, client3)
}

// TestWorktreeSurvivesDisconnectAndResumesOnReconnect is CLIENT-SERVER.md
// PR 2.4b's own scenario, the worktree gap TestWorktreeThroughDaemon used
// to leave undocumented: client A enters a worktree and starts a slow
// turn there, then disconnects entirely -- never calling ExitWorktree,
// never releasing the handle it holds -- leaving the worktree App
// running with nobody attached to it. Two things must both hold from
// there:
//   - the daemon must not exit on idle while that turn is still running,
//     even with no client connected at all (idle.go's busy check must
//     consult the orphaned worktree workspace, not just root);
//   - a fresh connection (client B) opening the same session must be
//     able to reach the worktree again through ResumeWorktree, see the
//     turn through to completion, and cleanly exit the worktree
//     afterward.
func TestWorktreeSurvivesDisconnectAndResumesOnReconnect(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("daemon e2e: skipped under -race, see racecheck_off_test.go")
	}

	const reply = "the resumed worktree turn finished"
	fixture := newFixtureServer(fixtureTurn{Text: reply, ChunkDelay: 500 * time.Millisecond})
	defer fixture.Close()

	projectDir := t.TempDir()
	gitInitRepo(t, projectDir)
	writeShortIdleTimeoutConfig(t, projectDir)

	dp := startDaemonProcess(t, projectDir, fixture.URL)
	ctx, cancel := context.WithTimeout(context.Background(), hangGuard)
	defer cancel()

	clientA, closeA := dp.dial(t)
	sess, err := clientA.CreateSession(ctx, "worktree resume test")
	require.NoError(t, err)
	require.NoError(t, clientA.SetCurrentSession(ctx, sess.ID))

	handleA, _, err := clientA.EnterWorktree(ctx, "feature")
	require.NoError(t, err)
	require.True(t, handleA.WorktreeState().Active)
	require.Equal(t, "feature", handleA.WorktreeState().Name)

	require.NoError(t, handleA.AgentRun(ctx, sess.ID, "answer slowly"))
	require.Eventually(t, func() bool {
		return handleA.AgentIsSessionBusy(sess.ID)
	}, hangGuard, 20*time.Millisecond, "turn in the worktree never became visibly busy")

	// Client A vanishes without ExitWorktree and without releasing the
	// handle it holds: the worktree App keeps running and keeps owning
	// the session, exactly like a dropped connection in production.
	closeA()

	// The daemon must not exit on idle while that turn is still running,
	// even though no client is connected at all right now -- the whole
	// point of PR 2.4b's fix to idle.go's busy check.
	select {
	case err := <-dp.waitCh:
		dp.waitCh <- err
		t.Fatalf("daemon exited while an orphaned worktree turn was still in flight (err=%v)", err)
	case <-time.After(2 * time.Second):
	}

	clientB, _ := dp.dial(t)
	require.NoError(t, clientB.SetCurrentSession(ctx, sess.ID))
	resumed, releaseB, err := clientB.ResumeWorktree(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, resumed.WorktreeState().Active)
	require.Equal(t, "feature", resumed.WorktreeState().Name)

	require.Eventually(t, func() bool {
		return !resumed.AgentIsSessionBusy(sess.ID)
	}, hangGuard, 50*time.Millisecond, "the resumed turn never finished")

	msgs, err := resumed.ListMessages(ctx, sess.ID)
	require.NoError(t, err)
	found := false
	for _, m := range msgs {
		if m.Role == message.Assistant && m.Content().Text == reply {
			found = true
		}
	}
	require.True(t, found, "expected the complete assistant reply %q among %d messages", reply, len(msgs))

	root, exitRelease, err := resumed.ExitWorktree(ctx)
	require.NoError(t, err)
	require.False(t, root.WorktreeState().Active)
	releaseB()
	exitRelease()

	shutdownCleanly(t, dp, clientB)
}
