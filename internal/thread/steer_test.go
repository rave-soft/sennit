package thread

import (
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// runtimeRunID reads the RunID the thread's live runtime currently treats
// as the owner of its workspace — the value handleRunComplete matches a
// completion against before tearing anything down. The steering path's
// whole correctness rests on when this does and does not move, so the
// tests below read it directly rather than inferring it from behavior.
func runtimeRunID(t *testing.T, mgr *Manager, id string) string {
	t.Helper()
	c := mgr.lc.existingControl(id)
	require.NotNil(t, c)
	c.mu.Lock()
	defer c.mu.Unlock()
	require.NotNil(t, c.runtime, "thread has no live runtime")
	return c.runtime.runID
}

// A message the person types into a thread's session reaches the turn the
// thread is already running, instead of waiting for it to end. That is the
// whole point of the person's path: a thread's turn can sit inside a
// sub-agent call for many minutes, and a correction read after those
// minutes has corrected nothing.
func TestManager_SendFromPersonFoldsIntoRunningTurn(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), CreateArgs{Name: "busy", Goal: "do it", MergePolicy: MergeManual})
	require.NoError(t, err)

	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, time.Millisecond)
	coord.setQueue(true, 2)
	goalRunID := runtimeRunID(t, mgr, st.ID)
	require.NotEmpty(t, goalRunID)

	disp, err := mgr.SendFromPerson(t.Context(), st.ID, "commit what you have first")
	require.NoError(t, err)
	require.True(t, disp.Steered, "a turn was in flight, so the message folded into it")
	require.False(t, disp.Queued, "folding is the opposite of queueing behind the turn")

	require.Eventually(t, func() bool { return coord.runCount() == 2 }, time.Second, time.Millisecond)
	coord.mu.Lock()
	folded := coord.runs[1]
	coord.mu.Unlock()
	require.Empty(t, folded.runID,
		"a folded message extends the turn in flight and has no run of its own to correlate")
	require.Empty(t, folded.origin,
		"the person typed this themselves, so the dispatch carries no agent-origin tag "+
			"and is persisted as their own words (an empty origin is message.OriginPerson)")

	// The folded message did not displace the goal run: that run is still
	// the one whose completion ends this thread. Moving the owner here
	// would strand the workspace on a run that never reports.
	require.Equal(t, goalRunID, runtimeRunID(t, mgr, st.ID))
}

// The same send into an idle session has nothing to fold into, so it
// becomes a turn of its own — and then it must take ownership of the
// workspace, exactly as an agent's send does, or nothing would ever settle
// the thread when that turn ends.
func TestManager_SendFromPersonStartsOwnTurnWhenIdle(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), CreateArgs{Name: "idle", Goal: "do it", MergePolicy: MergeManual})
	require.NoError(t, err)

	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, time.Millisecond)
	coord.setQueue(false, 0)

	disp, err := mgr.SendFromPerson(t.Context(), st.ID, "now do this instead")
	require.NoError(t, err)
	require.False(t, disp.Steered)
	require.False(t, disp.Queued)

	require.Eventually(t, func() bool { return coord.runCount() == 2 }, time.Second, time.Millisecond)
	coord.mu.Lock()
	own := coord.runs[1]
	coord.mu.Unlock()
	require.NotEmpty(t, own.runID)
	require.Empty(t, own.origin, "persisted as the person's own words, like any prompt they type")
	require.Equal(t, own.runID, runtimeRunID(t, mgr, st.ID),
		"the run that started owns the workspace from here")
}

// An agent's thread_send is unchanged by any of this: it still queues
// behind the turn in flight under its own RunID, and is still persisted as
// an agent dispatch. One agent interrupting another agent's turn is not a
// course correction, it is derailment — see Sender.
func TestManager_SendFromAgentStillQueuesBehindRunningTurn(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), CreateArgs{Name: "agentsend", Goal: "do it", MergePolicy: MergeManual})
	require.NoError(t, err)

	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, time.Millisecond)
	coord.setQueue(true, 1)
	goalRunID := runtimeRunID(t, mgr, st.ID)

	disp, err := mgr.Send(t.Context(), st.ID, "wrap up, you have five minutes")
	require.NoError(t, err)
	require.True(t, disp.Queued)
	require.Equal(t, 1, disp.Ahead)
	require.False(t, disp.Steered)

	require.Eventually(t, func() bool { return coord.runCount() == 2 }, time.Second, time.Millisecond)
	coord.mu.Lock()
	queued := coord.runs[1]
	coord.mu.Unlock()
	require.NotEmpty(t, queued.runID, "a queued follow-up runs as its own turn and reports under its own RunID")
	require.Equal(t, message.OriginAgent, queued.origin)
	require.Equal(t, queued.runID, runtimeRunID(t, mgr, st.ID),
		"the queued turn takes the workspace so the displaced run's completion cannot release it")
	require.NotEqual(t, goalRunID, queued.runID)
}
