package store

import (
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/stretchr/testify/require"
)

// TestNarrowUpdatesReportMissingSession ensures every narrow UPDATE preserves
// the service's missing-session contract instead of silently succeeding when
// SQLite affects no rows.
func TestNarrowUpdatesReportMissingSession(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn, dataDir)

	missing := "missing-session"
	tests := []struct {
		name string
		call func() error
	}{
		{"todos", func() error { return sessions.SetTodos(t.Context(), missing, nil) }},
		{"title and usage", func() error { return sessions.UpdateTitleAndUsage(t.Context(), missing, "title", 1, 2, 3) }},
		{"rename", func() error { return sessions.Rename(t.Context(), missing, "renamed") }},
		{"model", func() error {
			return sessions.SetModel(t.Context(), missing, session.ModelRef{Provider: "provider", Model: "model"})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updates := sessions.Subscribe(t.Context())
			require.ErrorIs(t, tt.call(), session.ErrNotFound)
			if tt.name == "rename" {
				select {
				case event := <-updates:
					t.Fatalf("missing Rename published unexpected %q update", event.Type)
				case <-time.After(100 * time.Millisecond):
				}
			}
		})
	}
}

// TestRenamePreservesUpdatedAt pins the guarantee Rename's doc comment
// makes. Renaming does not count as work on a session: updated_at orders
// ListSessions, decides what GetLastSession resumes, ages sessions out
// under gc, and feeds the time ProjectStatsSince reports, and a rename
// should move a session in none of those.
func TestRenamePreservesUpdatedAt(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn, dataDir)

	created, err := sessions.Create(t.Context(), "old session")
	require.NoError(t, err)
	setUpdatedAt(t, conn, created.ID, 10)

	require.NoError(t, sessions.Rename(t.Context(), created.ID, "renamed"))

	got, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, int64(10), got.UpdatedAt, "a title-only write must not bump updated_at")
	require.Equal(t, "renamed", got.Title)
}

// TestSaveStillBumpsUpdatedAt is the other half of the pair: narrowing the
// trigger to a column list must not stop a real write from bumping.
func TestSaveStillBumpsUpdatedAt(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn, dataDir)

	created, err := sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	setUpdatedAt(t, conn, created.ID, 10)

	created.Cost = 1.5
	_, err = sessions.Save(t.Context(), created)
	require.NoError(t, err)

	got, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.NotEqual(t, int64(10), got.UpdatedAt, "a usage write must still bump updated_at")
}

// TestSessionUpdatedAtTriggerCoversEveryWorkColumn guards the column list
// in 20260904000000_rename_does_not_bump_session_updated_at against the
// one way it can rot: a column added to sessions and not to the trigger
// would quietly stop bumping updated_at when written, and nothing else
// would notice. Two columns are excluded by design - title, which is what
// someone typed rather than work, and updated_at, whose explicit writers
// the trigger's WHEN guard already protects.
func TestSessionUpdatedAtTriggerCoversEveryWorkColumn(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	var triggerSQL string
	require.NoError(t, conn.QueryRowContext(t.Context(),
		`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = 'update_sessions_updated_at'`,
	).Scan(&triggerSQL))

	rows, err := conn.QueryContext(t.Context(), `SELECT name FROM pragma_table_info('sessions')`)
	require.NoError(t, err)
	defer rows.Close()

	excluded := map[string]bool{"title": true, "updated_at": true}
	var columns []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		columns = append(columns, name)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, columns)

	for _, name := range columns {
		if excluded[name] {
			require.NotRegexp(t, `(?m)^\s*`+name+`,?\s*$`, triggerSQL,
				"%q is excluded by design but appears in the trigger's column list", name)
			continue
		}
		require.Regexp(t, `(?m)^\s*`+name+`,?\s*$`, triggerSQL,
			"column %q is missing from update_sessions_updated_at: writes to it would not bump updated_at", name)
	}
}

// TestGetLastBreaksUpdatedAtTiesByID makes the query's deterministic ordering
// explicit. UUID-like IDs are strings, so descending ID is the stable winner.
func TestGetLastBreaksUpdatedAtTiesByID(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	queries := db.New(conn)
	sessions := NewService(queries, conn, dataDir)

	for _, id := range []string{"a", "z"} {
		_, err := queries.CreateSession(t.Context(), db.CreateSessionParams{ID: id, Title: id, ProjectPath: dataDir})
		require.NoError(t, err)
		setUpdatedAt(t, conn, id, 42)
	}
	last, err := sessions.GetLast(t.Context())
	require.NoError(t, err)
	require.Equal(t, "z", last.ID)
}

// TestDeletePublishesAnEventForEverySessionInTheTree pins what a delete
// tells its subscribers. The subtree is removed in one transaction
// because parent_session_id carries no foreign key, and for a long time
// only the root's removal was announced - so a delegation the person had
// stepped into stayed, as far as any subscriber could tell, alive. The UI
// compares the event's id against the session it is showing, missed, and
// kept sending turns to a row that no longer existed.
func TestDeletePublishesAnEventForEverySessionInTheTree(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	queries := db.New(conn)
	sessions := NewService(queries, conn, dataDir)

	parent, err := sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	child, err := sessions.CreateSubAgentSession(t.Context(), "call-1", parent.ID, "child", "coder")
	require.NoError(t, err)

	events := sessions.Subscribe(t.Context())
	require.NoError(t, sessions.Delete(t.Context(), parent.ID))

	seen := map[string]bool{}
	for range 2 {
		select {
		case event := <-events:
			if event.Type == pubsub.DeletedEvent {
				seen[event.Payload.ID] = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for the subtree's delete events")
		}
	}
	require.True(t, seen[child.ID], "the delegation's own removal must be announced")
	require.True(t, seen[parent.ID])
}
