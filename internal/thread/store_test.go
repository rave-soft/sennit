package thread

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) Store {
	t.Helper()
	return NewStoreForTest(t)
}

func testCreateParams(name string) CreateParams {
	return CreateParams{
		Name:         name,
		Goal:         "make the tests pass",
		BaseBranch:   "main",
		Branch:       "thread/" + name,
		WorktreePath: "/tmp/threads/" + name,
	}
}

// TestStore_CreateIDsSurviveASingleClockTick guards against a regression
// where thread.id was derived from a wall-clock timestamp
// (fmt.Sprintf("thread-%d", time.Now().UnixNano())): two Creates issued
// within the same clock tick got identical IDs and the second write hit
// the threads.id UNIQUE constraint — this is what actually happened in CI
// on Windows, whose default wall-clock granularity (~15.6ms) makes
// back-to-back calls landing in the same tick routine. A fast Linux clock
// makes that collision only occasional, so this pins it deterministically
// with synctest instead of hoping for bad luck: inside a synctest bubble,
// time.Now() does not advance between two straight-line calls with no
// blocking in between, which reproduces "same tick" on any platform. It
// passes today because Create no longer reads the clock at all (see
// store_testing.go), but it would fail again the moment ID generation
// went back to being time-derived.
func TestStore_CreateIDsSurviveASingleClockTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newTestStore(t)

		first, err := store.Create(t.Context(), testCreateParams("first"))
		require.NoError(t, err)
		second, err := store.Create(t.Context(), testCreateParams("second"))
		require.NoError(t, err, "second Create must not collide with the first even when the clock has not ticked")
		require.NotEqual(t, first.ID, second.ID)
	})
}

func TestStore_CreateAndGet(t *testing.T) {
	store := newTestStore(t)

	created, err := store.Create(t.Context(), testCreateParams("alpha"))
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)
	require.Equal(t, "alpha", created.Name)
	require.Equal(t, "make the tests pass", created.Goal)
	require.Equal(t, "main", created.BaseBranch)
	require.Equal(t, "thread/alpha", created.Branch)
	require.Equal(t, "/tmp/threads/alpha", created.WorktreePath)
	require.Equal(t, StatusPending, created.Status)
	require.Equal(t, MergeAuto, created.MergePolicy)
	require.Empty(t, created.SessionID)
	require.Zero(t, created.CompletedAt)
	require.NotZero(t, created.CreatedAt)
	require.NotZero(t, created.UpdatedAt)

	fetched, err := store.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, created, fetched)
}

func TestStore_CreateWithExplicitMergePolicy(t *testing.T) {
	store := newTestStore(t)

	params := testCreateParams("manual-merge")
	params.MergePolicy = MergeManual

	created, err := store.Create(t.Context(), params)
	require.NoError(t, err)
	require.Equal(t, MergeManual, created.MergePolicy)
}

func TestStore_GetByName(t *testing.T) {
	store := newTestStore(t)

	created, err := store.Create(t.Context(), testCreateParams("beta"))
	require.NoError(t, err)

	fetched, err := store.GetByName(t.Context(), "beta")
	require.NoError(t, err)
	require.Equal(t, created, fetched)

	_, err = store.GetByName(t.Context(), "does-not-exist")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestStore_DuplicateNameRejected(t *testing.T) {
	store := newTestStore(t)

	_, err := store.Create(t.Context(), testCreateParams("gamma"))
	require.NoError(t, err)

	_, err = store.Create(t.Context(), testCreateParams("gamma"))
	require.Error(t, err)
}

func TestStore_List(t *testing.T) {
	store := newTestStore(t)

	first, err := store.Create(t.Context(), testCreateParams("first"))
	require.NoError(t, err)
	second, err := store.Create(t.Context(), testCreateParams("second"))
	require.NoError(t, err)

	threads, err := store.List(t.Context())
	require.NoError(t, err)
	require.Len(t, threads, 2)
	// ListThreads orders by created_at, so insertion order is preserved.
	require.Equal(t, first.ID, threads[0].ID)
	require.Equal(t, second.ID, threads[1].ID)
}

func TestStore_SetStatusTransitions(t *testing.T) {
	store := newTestStore(t)

	created, err := store.Create(t.Context(), testCreateParams("delta"))
	require.NoError(t, err)

	running, err := store.SetStatus(t.Context(), created.ID, SetStatusParams{
		Status: StatusRunning,
	})
	require.NoError(t, err)
	require.Equal(t, StatusRunning, running.Status)
	require.Zero(t, running.CompletedAt)

	failed, err := store.SetStatus(t.Context(), created.ID, SetStatusParams{
		Status: StatusFailed,
		Error:  "worktree creation failed",
	})
	require.NoError(t, err)
	require.Equal(t, StatusFailed, failed.Status)
	require.Equal(t, "worktree creation failed", failed.Error)
	require.Zero(t, failed.CompletedAt)

	completed, err := store.SetStatus(t.Context(), created.ID, SetStatusParams{
		Status:        StatusCompleted,
		ResultSummary: "all good",
		CompletedAt:   1234567890,
	})
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, completed.Status)
	require.Equal(t, "all good", completed.ResultSummary)
	require.Equal(t, int64(1234567890), completed.CompletedAt)
	// Error from the previous transition should be cleared, since callers
	// pass the full desired state each time.
	require.Empty(t, completed.Error)
}

func TestStore_SetSession(t *testing.T) {
	store := newTestStore(t)

	created, err := store.Create(t.Context(), testCreateParams("epsilon"))
	require.NoError(t, err)
	require.Empty(t, created.SessionID)

	updated, err := store.SetSession(t.Context(), created.ID, "session-123")
	require.NoError(t, err)
	require.Equal(t, "session-123", updated.SessionID)

	fetched, err := store.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "session-123", fetched.SessionID)
}

func TestStore_Delete(t *testing.T) {
	store := newTestStore(t)

	created, err := store.Create(t.Context(), testCreateParams("zeta"))
	require.NoError(t, err)

	require.NoError(t, store.Delete(t.Context(), created.ID))

	_, err = store.Get(t.Context(), created.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestStore_GetNotFound(t *testing.T) {
	store := newTestStore(t)

	_, err := store.Get(t.Context(), "does-not-exist")
	require.ErrorIs(t, err, ErrNotFound)
}
