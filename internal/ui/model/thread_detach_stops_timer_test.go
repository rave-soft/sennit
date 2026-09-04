package model

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/stretchr/testify/require"
)

// TestThreadAttachmentStateRelease_StopsTheThreadsTurnTimer is the
// regression test for finding 4's detach half: common.StartTurn/StopTurn
// share a package-level table keyed by session ID across every *UI in the
// process, including a thread's embedded one. Destroying that embedded UI
// on detach did not touch the table, so a turn still in flight when the
// person detached stayed marked "started" forever — Elapsed(sessionID)
// would go on reporting time for a turn nobody would ever finish watching.
func TestThreadAttachmentStateRelease_StopsTheThreadsTurnTimer(t *testing.T) {
	t.Parallel()

	const threadSessionID = "thread-session-1"
	t.Cleanup(func() { common.StopTurn(threadSessionID) })
	common.StartTurn(threadSessionID)
	require.NotEqual(t, "", common.Elapsed(threadSessionID))

	threadUI := New(common.DefaultCommon(context.Background(), &rootTestWorkspace{}), "", false, WithEmbedded())
	threadUI.sess.current = &session.Session{ID: threadSessionID}

	state := threadAttachmentState{
		thread: &threadAttachment{threadID: "t1", ui: threadUI},
	}

	cmd := state.release()
	require.NotNil(t, cmd)
	cmd()

	require.Equal(t, "", common.Elapsed(threadSessionID), "detaching a thread must stop its own turn timer")
}
