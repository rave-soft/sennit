package thread_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/thread"
)

// TestManager_ShutdownCancelsReleaseWhenCallerGivesUp pins Shutdown's
// cleanup goroutine against a Spawner.Release that outlives the caller's
// patience. Shutdown used to hand that goroutine context.Background() for
// its whole lifetime — because m.ctx is already cancelled by the point the
// goroutine starts — so a caller whose ctx expired got back ctx.Err() while
// the release work kept running (and could hang) on an uncancellable
// context. Shutdown must instead give the goroutine a context of its own
// and cancel it the moment the waiting caller gives up.
func TestManager_ShutdownCancelsReleaseWhenCallerGivesUp(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	spawner.blockReleaseUntilCtxDone = true

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name: "hung-release", Goal: "go", MergePolicy: thread.MergeManual,
	})
	require.NoError(t, err)
	_, live := mgr.RuntimeForTest(st.ID)
	require.True(t, live, "the thread must own a live runtime for Shutdown to release")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = mgr.Shutdown(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded,
		"a caller whose ctx expires must get its own ctx.Err() back")

	require.Eventually(t, func() bool {
		return spawner.releaseCtxErrAt(st.WorktreePath) != nil
	}, eventuallyTimeout, eventuallyTick,
		"Release's context must itself be cancelled once the waiting caller gave up, "+
			"not left running on an uncancellable background context")
}

// TestManager_ShutdownReleasesOnLiveContextWhenCallerWaits is the successful-
// path counterpart to the test above: when Shutdown's cleanup finishes
// before the caller's ctx expires, Release must see a context that is still
// live for the whole call, not one pre-emptively cancelled.
func TestManager_ConcurrentShutdownKeepsCleanupAliveForRemainingCaller(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	spawner.releaseEntered = make(chan struct{})
	spawner.releaseBlock = make(chan struct{})

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name: "concurrent-shutdown", Goal: "go", MergePolicy: thread.MergeManual,
	})
	require.NoError(t, err)

	longDone := make(chan error, 1)
	go func() { longDone <- mgr.Shutdown(context.Background()) }()
	select {
	case <-spawner.releaseEntered:
	case <-time.After(eventuallyTimeout):
		t.Fatal("Release did not start")
	}

	shortCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	shortDone := make(chan error, 1)
	go func() { shortDone <- mgr.Shutdown(shortCtx) }()
	require.ErrorIs(t, <-shortDone, context.DeadlineExceeded)
	require.NoError(t, spawner.releaseCtxErrAt(st.WorktreePath))

	close(spawner.releaseBlock)
	require.NoError(t, <-longDone)
	require.Equal(t, 1, spawner.releases(st.WorktreePath))
	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusInterrupted, got.Status)
}

func TestManager_ShutdownReleasesOnLiveContextWhenCallerWaits(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name: "clean-release", Goal: "go", MergePolicy: thread.MergeManual,
	})
	require.NoError(t, err)

	require.NoError(t, mgr.Shutdown(t.Context()))
	require.NoError(t, spawner.releaseCtxErrAt(st.WorktreePath),
		"Release must be handed a live context when the caller waits out a clean shutdown")

	// A second Shutdown call, after the first already succeeded, must
	// still report success rather than the first call's now-cancelled
	// internal context.
	require.NoError(t, mgr.Shutdown(t.Context()))
}
