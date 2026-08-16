package db

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

// TestMigration_RenameStrandsToThreads verifies that a database created
// under the old "strands" table survives the rename migration intact:
// the row inserted under the old name reappears under "threads" with the
// same data, and the old table is gone.
func TestMigration_RenameStrandsToThreads(t *testing.T) {
	t.Cleanup(ResetPool)

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "sennit.db")

	conn, err := openDB(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	require.NoError(t, initGoose())

	// Migrate up to (and including) the migration that created the old
	// "strands" table, but no further.
	require.NoError(t, goose.UpTo(conn, "migrations", 20260810000000))

	_, err = conn.ExecContext(context.Background(), `
		INSERT INTO strands (
			id, name, goal, base_branch, branch, worktree_path,
			session_id, status, merge_policy, created_at, updated_at
		) VALUES (
			'st-1', 'alpha', 'do the thing', 'main', 'strand/alpha',
			'/tmp/worktree', 'sess-1', 'running', 'auto', 1000, 1000
		)`)
	require.NoError(t, err)

	// Migrate the rest of the way, applying the rename migration.
	require.NoError(t, goose.Up(conn, "migrations"))

	var exists int
	err = conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'strands'`).Scan(&exists)
	require.NoError(t, err)
	require.Equal(t, 0, exists, "old strands table should no longer exist")

	err = conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'threads'`).Scan(&exists)
	require.NoError(t, err)
	require.Equal(t, 1, exists, "threads table should exist")

	var (
		id, name, goal, baseBranch, branch, worktreePath, sessionID, status, mergePolicy string
		createdAt, updatedAt                                                             int64
	)
	err = conn.QueryRowContext(context.Background(), `
		SELECT id, name, goal, base_branch, branch, worktree_path,
			session_id, status, merge_policy, created_at, updated_at
		FROM threads WHERE id = 'st-1'`).Scan(
		&id, &name, &goal, &baseBranch, &branch, &worktreePath,
		&sessionID, &status, &mergePolicy, &createdAt, &updatedAt,
	)
	require.NoError(t, err, "row inserted into strands should be readable from threads")

	require.Equal(t, "st-1", id)
	require.Equal(t, "alpha", name)
	require.Equal(t, "do the thing", goal)
	require.Equal(t, "main", baseBranch)
	require.Equal(t, "strand/alpha", branch)
	require.Equal(t, "/tmp/worktree", worktreePath)
	require.Equal(t, "sess-1", sessionID)
	require.Equal(t, "running", status)
	require.Equal(t, "auto", mergePolicy)
	require.Equal(t, int64(1000), createdAt)
	require.Equal(t, int64(1000), updatedAt)
}
