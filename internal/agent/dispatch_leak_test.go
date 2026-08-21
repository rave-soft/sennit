package agent

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDispatcher_SessionStateReclaimedWhenIdle is the verification the
// mutex-leak fix needs: dispatch.go used to hold one *sync.Mutex per
// session id forever (see the removed dispatchMu map's doc comment). Now
// that a session's whole dispatch state lives in one refcounted
// sessionState, running N sessions to completion must leave dispatcher's
// states map empty afterwards - not just "not obviously growing", but
// actually zero, once every accepted run has finished and released its
// reference and each session's data (queue, completion inbox, active
// run, cancel mark) is genuinely idle.
func TestDispatcher_SessionStateReclaimedWhenIdle(t *testing.T) {
	t.Parallel()
	sa, env := newStreamTestAgent(t)

	const n = 50
	for i := range n {
		sess, err := env.sessions.Create(t.Context(), fmt.Sprintf("session-%d", i))
		require.NoError(t, err)

		result, err := sa.Run(t.Context(), SessionAgentCall{
			SessionID: sess.ID,
			Prompt:    "hello",
		})
		require.NoError(t, err)
		require.NotNil(t, result)
	}

	require.Equal(t, 0, sa.sessionCountForTest(),
		"every session's dispatch state must be reclaimed once its run completes and goes idle")
}

// TestDispatcher_SessionStateReclaimedAfterAcceptCycle covers the
// BeginAccepted/Close half of the state independently of a full Run: a
// session that only ever took accept reservations and closed them again
// (acceptedRuns and cancelMark both back to zero, nothing else ever
// touched) must not leave a dangling entry either.
func TestDispatcher_SessionStateReclaimedAfterAcceptCycle(t *testing.T) {
	t.Parallel()
	sa, _ := newCancelTestAgent(t)

	const n = 50
	for i := range n {
		sessionID := fmt.Sprintf("accept-only-%d", i)
		a := sa.BeginAccepted(sessionID)
		b := sa.BeginAccepted(sessionID)
		a.Close()
		b.Close()
	}

	require.Equal(t, 0, sa.sessionCountForTest(),
		"a session that only took and released accept reservations must not leak dispatch state")
}
