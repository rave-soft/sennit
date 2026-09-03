package thread_test

import (
	"testing"

	"github.com/rave-soft/sennit/internal/thread"

	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// runtimeRunID reads the RunID the thread's live runtime currently treats
// as the owner of its workspace — the value handleRunComplete matches a
// completion against before tearing anything down. The steering path's
// whole correctness rests on when this does and does not move, so the
// tests below read it directly rather than inferring it from behavior.
func runtimeRunID(t *testing.T, mgr *thread.Manager, id string) string {
	t.Helper()
	runID, live := mgr.RuntimeForTest(id)
	require.True(t, live, "thread has no live runtime")
	return runID
}

// A message the person types into a thread's session reaches the turn the
// thread is already running, instead of waiting for it to end. That is the
// whole point of the person's path: a thread's turn can sit inside a
// sub-agent call for many minutes, and a correction read after those
// minutes has corrected nothing.
func TestManager_SendFromPersonFoldsIntoRunningTurn(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "busy", Goal: "do it", MergePolicy: thread.MergeManual})
	require.NoError(t, err)

	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	coord.setQueue(true, 2)
	goalRunID := runtimeRunID(t, mgr, st.ID)
	require.NotEmpty(t, goalRunID)

	disp, err := mgr.SendFromPerson(t.Context(), st.ID, "commit what you have first")
	require.NoError(t, err)
	require.True(t, disp.Steered, "a turn was in flight, so the message folded into it")
	require.False(t, disp.Queued, "folding is the opposite of queueing behind the turn")

	require.Eventually(t, func() bool { return coord.runCount() == 2 }, eventuallyTimeout, eventuallyTick)
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

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "idle", Goal: "do it", MergePolicy: thread.MergeManual})
	require.NoError(t, err)

	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	coord.setQueue(false, 0)

	disp, err := mgr.SendFromPerson(t.Context(), st.ID, "now do this instead")
	require.NoError(t, err)
	require.False(t, disp.Steered)
	require.False(t, disp.Queued)

	require.Eventually(t, func() bool { return coord.runCount() == 2 }, eventuallyTimeout, eventuallyTick)
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

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "agentsend", Goal: "do it", MergePolicy: thread.MergeManual})
	require.NoError(t, err)

	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	coord.setQueue(true, 1)
	goalRunID := runtimeRunID(t, mgr, st.ID)

	disp, err := mgr.Send(t.Context(), st.ID, "wrap up, you have five minutes")
	require.NoError(t, err)
	require.True(t, disp.Queued)
	require.Equal(t, 1, disp.Ahead)
	require.False(t, disp.Steered)

	require.Eventually(t, func() bool { return coord.runCount() == 2 }, eventuallyTimeout, eventuallyTick)
	coord.mu.Lock()
	queued := coord.runs[1]
	coord.mu.Unlock()
	require.NotEmpty(t, queued.runID, "a queued follow-up runs as its own turn and reports under its own RunID")
	require.Equal(t, message.OriginAgent, queued.origin)
	require.Equal(t, queued.runID, runtimeRunID(t, mgr, st.ID),
		"the queued turn takes the workspace so the displaced run's completion cannot release it")
	require.NotEqual(t, goalRunID, queued.runID)
}

// A thread revived by hand and then worked in must show what it is
// actually doing. The drilled-in TUI used to talk to the thread's
// coordinator directly, so the manager never learned a turn had started:
// the thread sat at idle while its agent worked, and its completion was
// dropped on arrival. RunFromPerson is that path routed back through the
// manager.
func TestManager_RunFromPersonTracksTheTurnAndRestsAtIdle(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "revived", Goal: "do it", MergePolicy: thread.MergeAuto})
	require.NoError(t, err)
	worktree := st.WorktreePath

	// Let the goal run fail, then revive the thread the way attaching to
	// it does.
	publishFailure(t, spawner.appFor(worktree), st.SessionID, "stream error: overloaded")
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, settleTimeout))
	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusFailed, st.Status)

	st, err = mgr.Activate(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusIdle, st.Status)
	require.Equal(t, "stream error: overloaded", st.Error, "activation preserves what went wrong")

	// Reviving respawns the workspace, so the coordinator to watch is the
	// new one — its run counter starts empty.
	coord := spawner.coordFor(worktree)
	coord.setQueue(false, 0)
	_, err = mgr.RunFromPerson(t.Context(), st.ID, "carry on by hand", nil)
	require.NoError(t, err)

	// While the person's turn runs, the thread says so — the whole point.
	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusRunning, st.Status)
	require.Empty(t, st.Error, "a turn is in flight; the old failure is no longer the current state")

	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	coord.mu.Lock()
	personRun := coord.runs[0]
	coord.mu.Unlock()
	require.NotEmpty(t, personRun.runID, "the manager owns this turn, so it must be able to match its completion")

	// The turn ends: back to idle, workspace still live, nothing merged
	// and nothing reported — the person is still sitting in it.
	publishSuccess(t, spawner.appFor(worktree), st.SessionID)
	require.Eventually(t, func() bool {
		got, err := mgr.Get(t.Context(), st.ID)
		return err == nil && got.Status == thread.StatusIdle
	}, eventuallyTimeout, eventuallyTick)

	require.NotNil(t, mgr.Handle(st.ID), "the workspace stays live: the person is working in it")
	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Empty(t, got.Error, "the turn succeeded, so the stale failure is finally gone")
	require.Equal(t, thread.MergeAuto, got.MergePolicy)
	require.DirExists(t, worktree, "an auto-policy thread is not merged and discarded because the person stopped typing")
}

// publishFailure simulates a thread's agent run ending in an error, the
// state a thread revived by hand is usually found in.
func publishFailure(t *testing.T, a *app.App, sessionID, errText string) {
	t.Helper()
	coord := a.Coordinator().(*fakeCoordinator)
	require.Eventually(t, func() bool { return coord.runCount() > 0 }, eventuallyTimeout, eventuallyTick)
	coord.mu.Lock()
	runID := coord.runs[len(coord.runs)-1].runID
	coord.mu.Unlock()
	a.RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{SessionID: sessionID, RunID: runID, Error: errText})
}

// A cancel that lands between a person's dispatch being scheduled and the
// coordinator admitting it is the race the dispatch reserves acceptance
// for (see Coordinator.BeginAccepted). It must resolve to a definite
// answer: no run started, so nothing takes the workspace — and the
// delegation must not be left sitting at running with nothing in flight to
// ever move it off.
func TestManager_SendFromPersonCancelledOnEntryRestsAtIdle(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "raced", Goal: "do it", MergePolicy: thread.MergeManual})
	require.NoError(t, err)

	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	goalRunID := runtimeRunID(t, mgr, st.ID)
	coord.setQueue(false, 0)
	coord.setCancelOnEntry(true)

	disp, err := mgr.SendFromPerson(t.Context(), st.ID, "never mind")
	require.NoError(t, err, "a cancelled dispatch is an outcome, not a failure")
	require.False(t, disp.Steered)

	require.Equal(t, goalRunID, runtimeRunID(t, mgr, st.ID),
		"nothing ran, so nothing may take the workspace from the run that owns it")
	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusIdle, got.Status,
		"a live workspace with nothing in flight is idle, not running")
}

// TestManager_ParkedThreadSurvivesCancelledFollowUp is the regression test
// for a parked delegation losing its terminal completion (and, for an
// auto-merge thread, its merge) when a person cancels a follow-up sent
// while it was parked.
//
// A thread whose own turn finishes while its children are still running
// parks: its row is deliberately left at StatusRunning (see
// lifecycle.parkIfAwaitingDelegations) until its own delegations settle.
// If a person attaches to that still-"running" thread, sends a follow-up,
// and cancels it before it dispatches, the cancelled-on-entry path used to
// rest the entity at StatusIdle unconditionally — exactly like the
// ordinary case above. For a parked thread that clobber is fatal:
// finalizeRunComplete only reacts to a completion while the row still
// reads StatusRunning, so the real completion arriving later is silently
// dropped, and an auto-merge thread never merges.
func TestManager_ParkedThreadSurvivesCancelledFollowUp(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner, parentApp := newTestManagerWithParentApp(t, repo)

	parent, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:            "parent",
		Goal:            "coordinate",
		ParentSessionID: "parent-sess",
	})
	require.NoError(t, err)
	require.Equal(t, thread.MergeAuto, parent.MergePolicy)

	child, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:            "child",
		Goal:            "a piece of it",
		MergePolicy:     thread.MergeManual,
		ParentSessionID: parent.SessionID,
	})
	require.NoError(t, err)

	// The parent's own goal run finishes while the child is still running:
	// it must park rather than finalize.
	writeFile(t, parent.WorktreePath, "output.txt", "auto merged\n")
	publishSuccess(t, spawner.appFor(parent.WorktreePath), parent.SessionID)
	require.Eventually(t, func() bool {
		got, err := mgr.Get(t.Context(), parent.ID)
		return err == nil && got.Status == thread.StatusRunning
	}, eventuallyTimeout, eventuallyTick, "a parked thread's row stays running")
	require.NotNil(t, mgr.Handle(parent.ID), "a parked entity's workspace stays live")

	// A person attaches to what still reads as running and sends a
	// follow-up, then cancels it before it dispatches — the race
	// TestManager_SendFromPersonCancelledOnEntryRestsAtIdle covers for the
	// ordinary, non-parked case.
	coord := spawner.coordFor(parent.WorktreePath)
	coord.setQueue(false, 0)
	coord.setCancelOnEntry(true)
	disp, err := mgr.SendFromPerson(t.Context(), parent.ID, "never mind")
	require.NoError(t, err, "a cancelled dispatch is an outcome, not a failure")
	require.False(t, disp.Steered)

	got, err := mgr.Get(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusRunning, got.Status,
		"a cancelled follow-up must not clobber a parked entity's status")

	// The child settles, and the parent's real completion finally arrives.
	require.NoError(t, mgr.Cancel(t.Context(), child.ID, "no longer needed"))
	publishSuccess(t, spawner.appFor(parent.WorktreePath), parent.SessionID)

	require.Eventually(t, func() bool {
		got, err := mgr.Get(t.Context(), parent.ID)
		return err == nil && got.Status == thread.StatusMerged
	}, eventuallyTimeout, eventuallyTick,
		"the thread must reach a terminal status once its own delegations settle")

	parentCoord := parentApp.Coordinator().(*fakeCoordinator)
	require.Eventually(t, func() bool { return len(parentCoord.deliveredCompletions()) > 0 }, eventuallyTimeout, eventuallyTick,
		"its completion must be delivered")
}
