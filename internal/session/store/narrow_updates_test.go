package store

import (
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/db"
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
