package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

// beforeRepair is the last migration applied before
// 20260816000000_repair_file_versions_and_dangling_refs.
const beforeRepair = 20260815120000

// migrateToBeforeRepair opens a fresh database migrated up to, but not
// including, the repair migration, with two sessions in place for the
// child rows the tests attach to them.
func migrateToBeforeRepair(t *testing.T) *sql.DB {
	t.Helper()

	conn, err := openDB(filepath.Join(t.TempDir(), "sennit.db"))
	require.NoError(t, err)
	t.Cleanup(ResetPool)
	t.Cleanup(func() { conn.Close() })

	require.NoError(t, initGoose())
	require.NoError(t, goose.UpTo(conn, "migrations", beforeRepair))

	for _, id := range []string{"sess-a", "sess-b"} {
		_, err = conn.ExecContext(t.Context(), `
			INSERT INTO sessions (id, title, updated_at, created_at)
			VALUES (?, 'test', 1000, 1000)`, id)
		require.NoError(t, err)
	}
	return conn
}

// TestMigration_RepairRenumbersDuplicateFileVersions covers the state the
// old schema allowed and the new UNIQUE(path, version) forbids: two
// sessions each holding their own version 0 of one path. The migration
// has to renumber rather than fail, and it has to keep the recorded
// order, since that order is what ListBySessionTree and the UI's
// first-to-latest diff read.
func TestMigration_RepairRenumbersDuplicateFileVersions(t *testing.T) {
	conn := migrateToBeforeRepair(t)
	ctx := t.Context()

	insert := func(id, sessionID string, version, createdAt int64, content string) {
		t.Helper()
		_, err := conn.ExecContext(ctx, `
			INSERT INTO files (id, session_id, path, content, version, created_at, updated_at)
			VALUES (?, ?, 'main.go', ?, ?, ?, ?)`,
			id, sessionID, content, version, createdAt, createdAt)
		require.NoError(t, err)
	}
	insert("f-a0", "sess-a", 0, 100, "a first")
	insert("f-a1", "sess-a", 1, 200, "a second")
	insert("f-b0", "sess-b", 0, 300, "b first")

	require.NoError(t, goose.Up(conn, "migrations"))

	rows, err := conn.QueryContext(ctx,
		`SELECT id, version FROM files WHERE path = 'main.go' ORDER BY version`)
	require.NoError(t, err)
	defer rows.Close()
	got := map[string]int64{}
	var order []string
	for rows.Next() {
		var id string
		var version int64
		require.NoError(t, rows.Scan(&id, &version))
		got[id] = version
		order = append(order, id)
	}
	require.NoError(t, rows.Err())

	require.Equal(t, map[string]int64{"f-a0": 0, "f-a1": 1, "f-b0": 2}, got,
		"versions should be renumbered globally per path, in recorded order")
	require.Equal(t, []string{"f-a0", "f-a1", "f-b0"}, order)

	// The constraint that makes the renumbering worth doing.
	_, err = conn.ExecContext(ctx, `
		INSERT INTO files (id, session_id, path, content, version, created_at, updated_at)
		VALUES ('f-dup', 'sess-b', 'main.go', 'collides', 2, 400, 400)`)
	require.Error(t, err, "a second row may not claim a version already taken for the path")

	// A different path still starts its own numbering at zero.
	_, err = conn.ExecContext(ctx, `
		INSERT INTO files (id, session_id, path, content, version, created_at, updated_at)
		VALUES ('f-other', 'sess-b', 'other.go', 'fresh', 0, 400, 400)`)
	require.NoError(t, err)
}

// TestMigration_RepairDropsRedundantIndexes checks the two indexes whose
// leading column was already covered by a unique index or primary key.
func TestMigration_RepairDropsRedundantIndexes(t *testing.T) {
	conn := migrateToBeforeRepair(t)
	require.NoError(t, goose.Up(conn, "migrations"))

	indexes := map[string]bool{}
	rows, err := conn.QueryContext(t.Context(),
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name IS NOT NULL`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		indexes[name] = true
	}
	require.NoError(t, rows.Err())

	require.False(t, indexes["idx_files_path"])
	require.False(t, indexes["idx_read_files_path"])
	require.True(t, indexes["idx_files_session_id_path"])
	require.True(t, indexes["idx_files_created_at"])
	require.True(t, indexes["idx_threads_status"])
	require.True(t, indexes["idx_threads_project_path"])
	require.True(t, indexes["idx_read_files_session_id"])
}

// TestMigration_RepairThreadNameUniquePerKind pins the widened unique
// key: name lookups are kind-scoped, so two kinds sharing a name in one
// project must not collide, while two rows of the same kind still must.
func TestMigration_RepairThreadNameUniquePerKind(t *testing.T) {
	conn := migrateToBeforeRepair(t)
	ctx := t.Context()
	require.NoError(t, goose.Up(conn, "migrations"))

	insertThread := func(id, name, kind string) error {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO threads (
				id, name, project_path, goal, base_branch, branch, worktree_path,
				session_id, status, merge_policy, kind, created_at, updated_at
			) VALUES (?, ?, '/proj', 'goal', 'main', 'thread/x', '/wt', '', 'idle', 'auto', ?, 1000, 1000)`,
			id, name, kind)
		return err
	}

	require.NoError(t, insertThread("th-1", "alpha", "thread"))
	require.NoError(t, insertThread("tk-1", "alpha", "task"),
		"a task and a thread may share a name; nothing looks them up together")
	require.Error(t, insertThread("th-2", "alpha", "thread"),
		"two threads in one project still may not share a name")
}

// TestMigration_RepairClearsDanglingReferences covers the two pointers
// that reference a row without a foreign key behind them.
func TestMigration_RepairClearsDanglingReferences(t *testing.T) {
	conn := migrateToBeforeRepair(t)
	ctx := t.Context()
	require.NoError(t, goose.Up(conn, "migrations"))

	_, err := conn.ExecContext(ctx, `
		INSERT INTO messages (id, session_id, role, parts, created_at, updated_at)
		VALUES ('msg-1', 'sess-a', 'assistant', '[]', 1000, 1000)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx,
		`UPDATE sessions SET summary_message_id = 'msg-1' WHERE id = 'sess-a'`)
	require.NoError(t, err)

	_, err = conn.ExecContext(ctx, `
		INSERT INTO threads (
			id, name, project_path, goal, base_branch, branch, worktree_path,
			session_id, status, merge_policy, kind, parent_session_id, created_at, updated_at
		) VALUES ('th-1', 'alpha', '/proj', 'goal', 'main', 'thread/alpha', '/wt',
			'sess-a', 'idle', 'auto', 'thread', 'sess-b', 1000, 1000)`)
	require.NoError(t, err)

	// Deleting the summary message clears the pointer to it instead of
	// leaving the session pointing at a row that no longer exists.
	_, err = conn.ExecContext(ctx, `DELETE FROM messages WHERE id = 'msg-1'`)
	require.NoError(t, err)
	var summaryID sql.NullString
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT summary_message_id FROM sessions WHERE id = 'sess-a'`).Scan(&summaryID))
	require.False(t, summaryID.Valid, "summary_message_id should be cleared with its message")

	// Deleting the sessions resets both thread references to the ''
	// sentinel every reader already treats as "no session".
	_, err = conn.ExecContext(ctx, `DELETE FROM sessions WHERE id IN ('sess-a', 'sess-b')`)
	require.NoError(t, err)
	var sessionID, parentSessionID string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT session_id, parent_session_id FROM threads WHERE id = 'th-1'`).
		Scan(&sessionID, &parentSessionID))
	require.Empty(t, sessionID)
	require.Empty(t, parentSessionID)
}

// TestMigration_RepairDownUp verifies the Down migration restores a
// schema Up can be applied to again, so a rollback is not a one-way door.
func TestMigration_RepairDownUp(t *testing.T) {
	conn := migrateToBeforeRepair(t)
	ctx := t.Context()

	_, err := conn.ExecContext(ctx, `
		INSERT INTO files (id, session_id, path, content, version, created_at, updated_at)
		VALUES ('f-1', 'sess-a', 'main.go', 'body', 0, 100, 100)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `
		INSERT INTO threads (
			id, name, project_path, goal, base_branch, branch, worktree_path,
			session_id, status, merge_policy, kind, created_at, updated_at
		) VALUES ('th-1', 'alpha', '/proj', 'goal', 'main', 'thread/alpha', '/wt',
			'', 'idle', 'auto', 'thread', 1000, 1000)`)
	require.NoError(t, err)

	require.NoError(t, goose.Up(conn, "migrations"))
	require.NoError(t, goose.DownTo(conn, "migrations", beforeRepair))

	// The rows survive the round trip.
	var files, threads int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`).Scan(&files))
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads`).Scan(&threads))
	require.Equal(t, 1, files)
	require.Equal(t, 1, threads)

	require.NoError(t, goose.Up(conn, "migrations"))
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`).Scan(&files))
	require.Equal(t, 1, files)
}
