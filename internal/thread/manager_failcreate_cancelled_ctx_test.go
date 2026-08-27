package thread_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/thread"
)

// TestManager_FailCreateRecordsFailureOnCancelledContext pins failCreate
// against the same shape of bug handleRunComplete already had to be fixed
// for: it wrote the terminal failure status using the same caller ctx that
// had just failed. When that failure was ctx being cancelled, the write
// failed too and the row was left sitting at its transient status
// (StatusPending here — session creation fails before either idle or
// running is ever set) for the life of the process; only a restart's
// Recover would notice and fix it up.
func TestManager_FailCreateRecordsFailureOnCancelledContext(t *testing.T) {
	repo := initRepo(t)
	spawner := newFakeSpawner(t)
	spawner.sessionsErr = errors.New("session boom")

	ctx, cancel := context.WithCancel(t.Context())
	// Cancel ctx the moment Spawn hands back the workspace - after the row
	// is created and Spawn has succeeded, but before the session-creation
	// call that spawner.sessionsErr makes fail and routes into failCreate.
	spawner.afterSpawn = func(string) { cancel() }

	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       thread.NewStoreForTest(t),
		Spawner:     spawner,
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	shutdownManagerOnCleanup(t, mgr)

	_, err := mgr.Create(ctx, thread.CreateArgs{Name: "beta", Goal: "go", MergePolicy: thread.MergeManual})
	require.Error(t, err)
	require.ErrorContains(t, err, "session boom")

	st, err := mgr.Get(t.Context(), "beta")
	require.NoError(t, err)
	require.Equal(t, thread.StatusFailed, st.Status,
		"the terminal failure must be recorded even though the context that caused it was already cancelled")
	require.Contains(t, st.Error, "session boom")
}
