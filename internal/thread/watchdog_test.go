package thread_test

import (
	"context"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

// TestIdleWatchdog_EndsSilentTask is the whole point of the watchdog: a
// task whose run never terminates must not sit at running forever,
// holding a delegation slot its parent will never get back.
//
// The task here is created and then simply never publishes a completion —
// which is exactly what a wedged run looks like from this package's side,
// since a run's terminal event is the only way a task ever finishes.
func TestIdleWatchdog_EndsSilentTask(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "wedge", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	coord := parentApp.AgentCoordinator.(*fakeCoordinator)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)

	// One minute past the budget, measured from the task's own creation:
	// its session was never written to, so creation is what the sweep
	// measures against.
	tasks.SweepIdleTasksForTest(t.Context(), time.Unix(st.CreatedAt, 0).Add(thread.TaskIdleTimeoutForTest+time.Minute))

	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusFailed, got.Status,
		"a task that produced nothing must end as failed, not stay running")
	require.NotEmpty(t, got.Error, "the failure must record why it was ended")

	// The slot is only genuinely returned if the parent is told, so the
	// completion is as much a part of the fix as the status is.
	require.Eventually(t, func() bool { return len(coord.deliveredCompletions()) > 0 }, eventuallyTimeout, eventuallyTick)
	delivered := coord.deliveredCompletions()
	require.Len(t, delivered, 1)
	require.Equal(t, "parent-sess", delivered[0].sessionID)
	require.Equal(t, string(thread.StatusFailed), delivered[0].completion.Status)

	// And the wedged run itself is told to stop, so it is not left
	// burning tokens against a delegation nobody is waiting on.
	require.Eventually(t, func() bool {
		for _, s := range coord.canceledSessions() {
			if s == st.SessionID {
				return true
			}
		}
		return false
	}, eventuallyTimeout, eventuallyTick)
}

// TestIdleWatchdog_SparesTaskInsideBudget is the guard in the other
// direction, and the one that matters more: firing early kills work that
// was fine. A task that has not yet been silent for the full budget must
// be left strictly alone.
func TestIdleWatchdog_SparesTaskInsideBudget(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "slow but alive", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	coord := parentApp.AgentCoordinator.(*fakeCoordinator)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)

	tasks.SweepIdleTasksForTest(t.Context(), time.Unix(st.CreatedAt, 0).Add(thread.TaskIdleTimeoutForTest-time.Minute))

	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusRunning, got.Status,
		"a task inside its budget must not be touched")
	require.Empty(t, coord.deliveredCompletions(), "nothing may be reported for a task that is still running")
	require.Empty(t, coord.canceledSessions(), "a task inside its budget must not be cancelled")
}

// TestIdleWatchdog_SparesTaskAwaitingPermission covers the failure mode
// that would make this watchdog worse than no watchdog: a delegation
// blocked on a permission prompt is silent for as long as the person
// takes to answer, and that wait has no upper bound by design. Killing
// those would mean killing precisely the work the user was about to
// approve.
func TestIdleWatchdog_SparesTaskAwaitingPermission(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "needs approval", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	coord := parentApp.AgentCoordinator.(*fakeCoordinator)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)

	// Raise a real request tagged with this task's delegation identity,
	// exactly as a task's own tool call does (see TaskManager.Create's
	// permission.WithDelegation), and leave it unanswered.
	perms := parentApp.Permissions()
	askCtx, cancelAsk := context.WithCancel(context.Background())
	defer cancelAsk()
	go func() {
		//nolint:errcheck // The request is abandoned by cancelAsk; its answer is never read.
		perms.Request(permission.WithDelegation(askCtx, permission.DelegationRef{
			ID: st.ID, Name: st.Name, Kind: string(thread.KindTask),
		}), permission.CreatePermissionRequest{
			SessionID: st.SessionID, ToolName: "bash", Action: "execute", Path: t.TempDir(),
		})
	}()
	require.Eventually(t, func() bool { return perms.AwaitingAnswer(st.ID) }, eventuallyTimeout, eventuallyTick)

	tasks.SweepIdleTasksForTest(t.Context(), time.Unix(st.CreatedAt, 0).Add(thread.TaskIdleTimeoutForTest+time.Hour))

	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusRunning, got.Status,
		"a task waiting on a person must never be ended for being silent")
	require.Empty(t, coord.canceledSessions())
}

// TestIdleWatchdog_IgnoresFinishedTask proves the sweep only ever judges
// live work. A task that already finished has no runtime and no slot to
// reclaim, and re-finalizing one would deliver its parent a second,
// contradictory completion for work it was already told about.
func TestIdleWatchdog_IgnoresFinishedTask(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, parentApp := newTestTaskManager(t, store)

	st, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "done already", ParentSessionID: "parent-sess"})
	require.NoError(t, err)
	coord := parentApp.AgentCoordinator.(*fakeCoordinator)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
	publishSuccess(t, parentApp, st.SessionID)
	require.Eventually(t, func() bool {
		got, err := store.Get(t.Context(), st.ID)
		return err == nil && got.Status == thread.StatusCompleted
	}, eventuallyTimeout, eventuallyTick)

	tasks.SweepIdleTasksForTest(t.Context(), time.Unix(st.CreatedAt, 0).Add(thread.TaskIdleTimeoutForTest+time.Hour))

	got, err := store.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusCompleted, got.Status, "a finished task must be left as it finished")
	require.Len(t, coord.deliveredCompletions(), 1, "a finished task must not be reported twice")
}
