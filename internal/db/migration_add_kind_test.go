package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

// TestMigration_AddKindToThreads verifies that a database created before
// the kind column existed survives the migration intact: a pre-existing
// row backfills to "thread", a new row can set kind explicitly, and the
// Down migration actually drops the column rather than merely declaring
// intent to.
func TestMigration_AddKindToThreads(t *testing.T) {
	t.Cleanup(ResetPool)

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "sennit.db")

	conn, err := openDB(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	require.NoError(t, initGoose())

	// Migrate up to (and including) the last migration before kind was
	// added, and insert a row the way a database created before this
	// migration would have it: no kind column at all.
	require.NoError(t, goose.UpTo(conn, "migrations", 20260811000001))
	_, err = conn.ExecContext(context.Background(), `
		INSERT INTO threads (
			id, name, project_path, goal, base_branch, branch, worktree_path,
			session_id, status, merge_policy, created_at, updated_at
		) VALUES (
			'th-1', 'alpha', '', 'do the thing', 'main', 'thread/alpha',
			'/tmp/worktree', 'sess-1', 'running', 'auto', 1000, 1000
		)`)
	require.NoError(t, err)

	// Apply the kind migration.
	require.NoError(t, goose.Up(conn, "migrations"))

	var kind string
	err = conn.QueryRowContext(context.Background(),
		`SELECT kind FROM threads WHERE id = 'th-1'`).Scan(&kind)
	require.NoError(t, err)
	require.Equal(t, "thread", kind, "pre-existing rows should backfill to the thread kind")

	// A new row can set kind explicitly, the way CreateThread does today
	// (kind='thread') and a future task store would (kind='task').
	_, err = conn.ExecContext(context.Background(), `
		INSERT INTO threads (
			id, name, project_path, goal, base_branch, branch, worktree_path,
			session_id, status, merge_policy, kind, created_at, updated_at
		) VALUES (
			'th-2', 'beta', '', 'do another thing', 'main', 'thread/beta',
			'/tmp/worktree2', 'sess-2', 'running', 'auto', 'task', 1000, 1000
		)`)
	require.NoError(t, err)
	err = conn.QueryRowContext(context.Background(),
		`SELECT kind FROM threads WHERE id = 'th-2'`).Scan(&kind)
	require.NoError(t, err)
	require.Equal(t, "task", kind)

	// Down actually removes the column rather than being a no-op.
	require.NoError(t, goose.DownTo(conn, "migrations", 20260811000001))
	rows, err := conn.QueryContext(context.Background(), `PRAGMA table_info(threads)`)
	require.NoError(t, err)
	defer rows.Close()
	sawKind := false
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, colType    string
			dflt             sql.NullString
		)
		require.NoError(t, rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk))
		if name == "kind" {
			sawKind = true
		}
	}
	require.NoError(t, rows.Err())
	require.False(t, sawKind, "Down should have dropped the kind column")

	// Re-applying Up should restore the column and backfill existing rows
	// the same way it did the first time.
	require.NoError(t, goose.Up(conn, "migrations"))
	err = conn.QueryRowContext(context.Background(),
		`SELECT kind FROM threads WHERE id = 'th-1'`).Scan(&kind)
	require.NoError(t, err)
	require.Equal(t, "thread", kind)
}
