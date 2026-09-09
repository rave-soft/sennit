package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

func TestMigrationCollapseMergeStatuses(t *testing.T) {
	conn, err := openDB(filepath.Join(t.TempDir(), "sennit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	require.NoError(t, initGoose())
	require.NoError(t, goose.UpTo(conn, "migrations", 20260909000000))

	for _, status := range []string{"merging", "merged", "conflict", "merge_blocked"} {
		_, err = conn.ExecContext(t.Context(), `INSERT INTO threads
			(id, name, project_path, goal, base_branch, branch, worktree_path, status,
			 merge_policy, created_at, updated_at, kind)
			VALUES (?, ?, 'project', 'goal', 'main', ?, ?, ?, 'auto', 1, 1, 'thread')`,
			status, status, "thread/"+status, filepath.Join(t.TempDir(), status), status)
		require.NoError(t, err)
	}

	require.NoError(t, goose.Up(conn, "migrations"))
	rows, err := conn.QueryContext(t.Context(), `SELECT status FROM threads ORDER BY name`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var status string
		require.NoError(t, rows.Scan(&status))
		require.Equal(t, "completed", status)
	}
	require.NoError(t, rows.Err())

	var mergePolicyColumns int
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pragma_table_info('threads') WHERE name = 'merge_policy'`).Scan(&mergePolicyColumns))
	require.Zero(t, mergePolicyColumns)

	require.NoError(t, goose.DownTo(conn, "migrations", 20260909000000))
	var defaultPolicy sql.NullString
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT dflt_value FROM pragma_table_info('threads') WHERE name = 'merge_policy'`).Scan(&defaultPolicy))
	require.True(t, defaultPolicy.Valid)
}
