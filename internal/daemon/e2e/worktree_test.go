package e2e

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWorktreeThroughDaemon covers CLIENT-SERVER.md PR 2.4's worktree
// scenario: entering and exiting a worktree over the daemon (real
// EnterWorktree/ExitWorktree RPCs, a real git repo, a real second worktree
// directory on disk), and that the transfer is really exclusive -- a
// second, independently dialed connection cannot also claim the same
// session while the first holds it.
//
// What this does NOT cover: a client that enters a worktree and then
// drops its connection entirely (rather than calling ExitWorktree)
// before a new connection tries to resume it. Handles are torn down once
// their owning client goes quiet (internal/workspace/wsrpc/grpcws/
// handles.go's leaseManager, CLIENT-SERVER.md PR 1.3), but the worktree
// App instance behind that handle keeps running -- there is no RPC in
// this codebase today that lets a fresh connection re-attach to an
// already-entered worktree the way AttachThread does for a background
// thread. Reaching that path in this package would just be timing out
// waiting for something the wire protocol has no way to ask for; see this
// test's own doc comment history/report for the finding, which is a
// production gap worth its own follow-up rather than something to paper
// over here with a synthetic wait.
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
