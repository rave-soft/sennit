package thread_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/thread"
)

// TestManager_PersonSendThatNeverDispatchesRestsTheThread pins the state a
// failed person-driven turn leaves behind. send moves the delegation to
// running before dispatching, because it has no way to know the dispatch
// will fail — and when the coordinator never admitted the call at all, no
// run existed to move it back. The row sat at running for the rest of the
// process: the dashboard showed it working, Merge refused it as active,
// and nothing would ever complete it.
func TestManager_PersonSendThatNeverDispatchesRestsTheThread(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{Name: "typed-into", MergePolicy: thread.MergeManual})
	require.NoError(t, err)
	require.Equal(t, thread.StatusIdle, st.Status)

	// A coordinator that fails the dispatch outright, reporting no
	// decision — the one shape lifecycle.steer's error branch is for.
	coord := spawner.coordFor(st.WorktreePath)
	coord.steerDispatchErr = errors.New("coordinator said no")

	_, err = mgr.RunFromPerson(t.Context(), st.ID, "type type type", nil)
	require.Error(t, err, "the caller must be told the dispatch failed")

	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.NotEqual(t, thread.StatusRunning, got.Status,
		"a turn that never dispatched must not leave the thread running")
	require.Equal(t, thread.StatusIdle, got.Status)
}
