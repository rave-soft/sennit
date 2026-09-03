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

	a := &app.App{}
	a.SetSessionsForTest(sessions)
	store := configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(t.TempDir()))
	ws := NewAppWorkspace(a, store)

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
