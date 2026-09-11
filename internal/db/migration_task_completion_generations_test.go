package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

func TestMigrationTaskCompletionGenerations(t *testing.T) {
	conn, err := openDB(filepath.Join(t.TempDir(), "sennit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	require.NoError(t, initGoose())
	require.NoError(t, goose.UpTo(conn, "migrations", 20260908000000))
	for _, fixture := range []struct {
		id       string
		terminal any
		pending  int
	}{
		{"explicit", int64(20), 1},
		{"missing", nil, 1},
		{"delivered", int64(30), 0},
	} {
		_, err = conn.ExecContext(t.Context(), `INSERT INTO threads
			(id, name, project_path, goal, base_branch, branch, worktree_path, session_id,
			status, merge_policy, result_summary, error, created_at, updated_at, completed_at,
			kind, parent_session_id, completion_pending, completion_depth, terminal_at)
			VALUES (?, ?, 'project', 'goal', '', '', '', 'child', 'failed', 'manual',
			'result', 'error', 10, 12, 11, 'task', 'parent', ?, 3, ?)`, fixture.id, fixture.id, fixture.pending, fixture.terminal)
		require.NoError(t, err)
	}
	require.NoError(t, goose.Up(conn, "migrations"))
	queries := New(conn)
	pending, err := queries.ListPendingTaskCompletions(t.Context(), "project")
	require.NoError(t, err)
	require.Len(t, pending, 2)
	for _, snapshot := range pending {
		current, err := queries.GetThread(t.Context(), snapshot.ID)
		require.NoError(t, err)
		require.True(t, current.TerminalAt.Valid)
		require.Equal(t, current.TerminalAt.Int64, snapshot.TerminalAt_2)
		require.Equal(t, "failed", snapshot.Status_2)
		require.Equal(t, "result", snapshot.ResultSummary_2)
		require.Equal(t, "error", snapshot.Error_2)
		require.Equal(t, int64(3), snapshot.CompletionDepth_2)
		require.Equal(t, sql.NullInt64{Int64: 11, Valid: true}, snapshot.CompletedAt_2)
		require.Equal(t, snapshot.ID, snapshot.Name_2)
		require.Equal(t, "goal", snapshot.Goal_2)
		require.Equal(t, "parent", snapshot.ParentSessionID_2)
		require.Equal(t, "child", snapshot.SessionID_2)
		if snapshot.ID == "missing" {
			require.Equal(t, int64(12000000000), snapshot.TerminalAt_2)
		} else {
			require.Equal(t, int64(20), snapshot.TerminalAt_2)
		}
		_, err = conn.ExecContext(t.Context(), `UPDATE threads SET status = 'running' WHERE id = ?`, snapshot.ID)
		require.NoError(t, err)
		final, err := queries.FinalizeTask(t.Context(), FinalizeTaskParams{
			ID: snapshot.ID, SessionID: "child", ParentSessionID: "parent", Status: "completed",
			TerminalAt: sql.NullInt64{Int64: 1, Valid: true},
		})
		require.NoError(t, err)
		require.Equal(t, snapshot.TerminalAt_2+1, final.TerminalAt.Int64)
	}
}
