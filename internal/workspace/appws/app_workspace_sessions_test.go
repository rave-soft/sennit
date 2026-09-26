package appws

import (
	"testing"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/session"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/stretchr/testify/require"
)

// TestRenameSession_PreservesConcurrentUsageAndTodos reproduces G3: the
// sessions dialog holds a ListSessions snapshot taken when it opened and,
// with no subscription to session updates, that snapshot can be arbitrarily
// stale by the time the user confirms a rename. Renaming through
// SaveSession would write that whole stale row back, silently erasing
// cost, todos and summary_message_id that other writers set in the
// meantime. RenameSession must touch only the title.
func TestRenameSession_PreservesConcurrentUsageAndTodos(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := sessionstore.NewService(db.New(conn), conn, dataDir)

	created, err := sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	// The dialog's snapshot: taken before any usage, todos or summary
	// land, exactly like ListSessions at dialog-open time.
	staleSnapshot := created

	// Other writers land while the rename dialog is still open, the way a
	// turn finishing, the todo tool, or auto-summarization would.
	_, err = sessions.SaveUsage(t.Context(), session.Session{
		ID:               created.ID,
		Title:            created.Title,
		SummaryMessageID: "summary-msg-1",
		Todos:            []session.Todo{{Content: "write the fix", Status: session.TodoStatusPending}},
	}, 1.5)
	require.NoError(t, err)

	// app.NewForTest, not a bare &app.App{}: SENNIT_TEST_WIRE=grpc's
	// grpcws.Client.Connect reads the full class-C surface up front
	// (wsrpc.BuildClientState), including PermissionSkipRequests and the
	// root event hub's eager SubscribeWith (CLIENT-SERVER.md, PR 1.4a) --
	// both of which need a real permissions service and events broker, a
	// bare App leaves nil. Under SENNIT_TEST_WIRE=1 (wsrpc.NewLoopback)
	// this was never exercised, since Loopback never reads ahead of what
	// a test actually calls.
	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	a.SetSessionsForTest(sessions)
	store := configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(t.TempDir()))
	// wireWorkspace: RenameSession below is the only call made through ws;
	// everything else this test asserts on comes from sessions directly, so
	// it's a clean candidate for the wire CI job -- see wiretest_test.go.
	ws := wireWorkspace(t, NewAppWorkspace(a, store))

	// Confirming the rename in the dialog only ever had staleSnapshot's
	// title to offer; RenameSession must not carry the rest of that stale
	// row along with it.
	err = ws.RenameSession(t.Context(), staleSnapshot.ID, "renamed by user")
	require.NoError(t, err)

	got, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "renamed by user", got.Title)
	require.Equal(t, 1.5, got.Cost, "RenameSession must not erase cost a concurrent usage save wrote")
	require.Equal(t, "summary-msg-1", got.SummaryMessageID, "RenameSession must not roll back the summary pointer")
	require.Len(t, got.Todos, 1, "RenameSession must not roll back todos")
}

// TestCreateSession_ClearsAgentToolCache pins CreateSession as the new home
// for the process-wide grep/glob regex cache reset that used to be a
// separate Workspace method (ResetAgentToolCache) the UI called by hand
// from newSession. Folding it into CreateSession means every session gets
// a clean cache regardless of caller, not just the one frontend that
// remembered to ask for it.
//
// It swaps the package-level resetToolCache seam for a counting stand-in
// rather than reaching into internal/agent/tools's cache directly — that
// package must not carry test-only exports (see commit 59666e391). This
// mutates shared package state, so unlike its neighbor above it must NOT
// run in parallel with another test that swaps the same var; there is none
// today (TestAppWorkspace_AgentRunShellCommand_StreamingErrorSurfaces swaps
// a different var, runAndCaptureStream, and doesn't call t.Parallel()
// either), but this test deliberately omits t.Parallel() to keep it that
// way.
func TestCreateSession_ClearsAgentToolCache(t *testing.T) {
	orig := resetToolCache
	t.Cleanup(func() { resetToolCache = orig })
	calls := 0
	resetToolCache = func() { calls++ }

	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := sessionstore.NewService(db.New(conn), conn, dataDir)

	a := &app.App{}
	a.SetSessionsForTest(sessions)
	store := configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(t.TempDir()))
	ws := NewAppWorkspace(a, store)

	_, err = ws.CreateSession(t.Context(), "New Session")
	require.NoError(t, err)

	require.Equal(t, 1, calls, "CreateSession must clear the grep/glob regex cache")
}
