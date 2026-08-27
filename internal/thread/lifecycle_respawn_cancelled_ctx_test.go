package thread_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/thread"
)

// TestManager_Send_ReleasesRespawnedHandleOnCancelledContext pins the
// respawn-then-dispatch branch of lifecycle.send against a caller context
// that is already cancelled by the time setStatus runs. That branch used
// to defer spawner.Release(ctx, ...) on the caller's own ctx: when
// setStatus failed precisely because ctx was already dead, the deferred
// Release inherited the same dead ctx and failed too, leaking the freshly
// spawned App (and its DB connection) for the life of the process.
func TestManager_Send_ReleasesRespawnedHandleOnCancelledContext(t *testing.T) {
	repo := initRepo(t)
	store := thread.NewStoreForTest(t)
	parentApp := newTestParentApp(t)

	mgr1 := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     newFakeSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
		ParentApp:   &testAppWorkspace{app: parentApp},
	})
	shutdownManagerOnCleanup(t, mgr1)
	st, err := mgr1.Create(t.Context(), thread.CreateArgs{
		Name:            "cancel-on-resume",
		Goal:            "do it",
		MergePolicy:     thread.MergeManual,
		ParentSessionID: "parent-sess",
	})
	require.NoError(t, err)

	// A fresh Manager, standing in for a restarted process: it has no live
	// runtime for st, so Send below must respawn the workspace before it
	// can dispatch.
	spawner2 := newFakeSpawner(t)
	mgr2 := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     spawner2,
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
		ParentApp:   &testAppWorkspace{app: parentApp},
	})
	shutdownManagerOnCleanup(t, mgr2)

	// Cancel the caller's own ctx the moment the respawn's Spawn call
	// returns - after resolve has already used it to look up st, but
	// before send's own setStatus call, which is exactly the window the
	// deferred Release used to inherit a dead ctx from. See afterSpawn's
	// doc comment for why this hook is the only deterministic way to land
	// the cancel there.
	ctx, cancel := context.WithCancel(t.Context())
	spawner2.afterSpawn = func(string) { cancel() }

	err = sendErr(mgr2.Send(ctx, st.ID, "resume message"))
	require.ErrorIs(t, err, context.Canceled)

	require.True(t, spawner2.wasReleased(st.WorktreePath),
		"the freshly spawned handle must be released even though the caller's context was already cancelled")
	require.NoError(t, spawner2.releaseCtxErrAt(st.WorktreePath),
		"Release must be handed a context that outlives the caller's own cancellation")
}
