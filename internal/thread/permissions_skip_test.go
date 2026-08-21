package thread_test

import (
	"testing"

	"github.com/rave-soft/sennit/internal/thread"

	"github.com/stretchr/testify/require"
)

// TestManager_SetPermissionsSkipReachesLiveThreads: a thread runs in an
// isolated app.App with its own permission service, so it only learns the
// parent's permission-bypass ("yolo") state when it is spawned. Toggling
// yolo in the main window used to leave a running thread on whatever the
// state was at spawn time — stuck waiting on a prompt after the user
// turned bypass on, or, worse, still skipping prompts after they turned it
// off.
func TestManager_SetPermissionsSkipReachesLiveThreads(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:        "yolo-follower",
		Goal:        "implement the thing",
		MergePolicy: thread.MergeManual,
	})
	require.NoError(t, err)

	handle := spawner.handleFor(st.WorktreePath)
	require.NotNil(t, handle)
	require.False(t, handle.App().Permissions().SkipRequests(),
		"precondition: the spawned thread starts without bypass")

	mgr.SetPermissionsSkip(true)
	require.True(t, handle.App().Permissions().SkipRequests(),
		"turning bypass on in the parent must reach a thread already running")

	mgr.SetPermissionsSkip(false)
	require.False(t, handle.App().Permissions().SkipRequests(),
		"turning bypass off must reach it too — this is the direction with consequences")
}

// TestManager_SetPermissionsSkipIgnoresThreadsWithNoLiveWorkspace guards
// the walk itself: a delegation that has finished (or never started) has
// no runtime, and reaching through a nil one would panic on a toggle.
func TestManager_SetPermissionsSkipIgnoresThreadsWithNoLiveWorkspace(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:        "cancelled",
		Goal:        "implement the thing",
		MergePolicy: thread.MergeManual,
	})
	require.NoError(t, err)
	require.NoError(t, mgr.Cancel(t.Context(), st.ID, "done with it"))

	require.NotPanics(t, func() { mgr.SetPermissionsSkip(true) })
}
