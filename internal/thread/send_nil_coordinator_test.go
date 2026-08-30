package thread_test

import (
	"testing"

	"github.com/rave-soft/sennit/internal/thread"

	"github.com/stretchr/testify/require"
)

// A workspace can present a nil Coordinator — it is documented as possible
// (an App with no agent configured) and threadspawn's adapter really
// returns nil before setup and after a rebuild closes it. startRun has
// always guarded against that, but send did not: the person's branch
// reached steerDispatch, which called BeginAccepted on the nil straight
// away and panicked on the caller's own goroutine, and the agent's branch
// discovered the nil only after it had already written StatusRunning and
// advanced rt.runID — leaving the entity marked running with no run left
// to publish the RunComplete that would clear it.
//
// Both branches now resolve the coordinator once, before anything is
// committed.
func TestManager_SendWithNilCoordinatorFailsWithoutStrandingStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(mgr *thread.Manager, id string) (thread.SendDisposition, error)
	}{
		{"person", func(mgr *thread.Manager, id string) (thread.SendDisposition, error) {
			return mgr.SendFromPerson(t.Context(), id, "steer it")
		}},
		{"agent", func(mgr *thread.Manager, id string) (thread.SendDisposition, error) {
			return mgr.Send(t.Context(), id, "queue it")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t)
			mgr, spawner := newTestManager(t, repo)

			st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "nil-coord-" + tc.name, Goal: "do it", MergePolicy: thread.MergeManual})
			require.NoError(t, err)

			coord := spawner.coordFor(st.WorktreePath)
			require.Eventually(t, func() bool { return coord.runCount() == 1 }, eventuallyTimeout, eventuallyTick)
			coord.setQueue(false, 0)

			before, err := mgr.Get(t.Context(), st.ID)
			require.NoError(t, err)

			// The runtime stays live, but the coordinator goes away
			// underneath it — exactly what a rebuild closing the App's
			// coordinator leaves behind.
			spawner.handleFor(st.WorktreePath).App().SetAgentCoordinatorForTest(nil)

			// The point of the test: an error, not a panic.
			_, err = tc.send(mgr, st.ID)
			require.Error(t, err)
			require.Contains(t, err.Error(), "no agent coordinator")

			after, err := mgr.Get(t.Context(), st.ID)
			require.NoError(t, err)
			require.Equal(t, before.Status, after.Status,
				"a send that never dispatched must not leave the entity marked running")
		})
	}
}
