package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// openConstraintTestDB opens a real, migrated SQLite connection in a
// throwaway data directory, using the same Connect path production code
// does. The constraint checks below need real driver errors, not
// hand-built ones, since the whole point is verifying that each driver's
// actual error type is recognized correctly.
func openConstraintTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dataDir := t.TempDir()
	t.Cleanup(ResetPool)

	conn, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, Release(dataDir)) })
	return conn
}

// These tests do not call t.Parallel(): ResetPool (used via
// openConstraintTestDB's cleanup) tears down the entire process-wide
// connection pool, not just this test's data dir, so it would race with
// any sibling test still mid-Connect. connect_test.go follows the same
// convention for the same reason.
func TestIsUniqueConstraintError(t *testing.T) {
	conn := openConstraintTestDB(t)
	ctx := context.Background()

	_, err := conn.ExecContext(ctx, `CREATE TABLE unique_probe (name TEXT UNIQUE)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `INSERT INTO unique_probe (name) VALUES ('a')`)
	require.NoError(t, err)

	_, dupErr := conn.ExecContext(ctx, `INSERT INTO unique_probe (name) VALUES ('a')`)
	require.Error(t, dupErr)

	require.True(t, IsUniqueConstraintError(dupErr))
	require.False(t, IsForeignKeyConstraintError(dupErr))

	require.False(t, IsUniqueConstraintError(nil))
	require.False(t, IsUniqueConstraintError(errors.New("boom")))
}

func TestIsForeignKeyConstraintError(t *testing.T) {
	conn := openConstraintTestDB(t)
	ctx := context.Background()

	_, err := conn.ExecContext(ctx, `CREATE TABLE fk_parent (id INTEGER PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `CREATE TABLE fk_child (
		parent_id INTEGER NOT NULL REFERENCES fk_parent(id)
	)`)
	require.NoError(t, err)

	_, fkErr := conn.ExecContext(ctx, `INSERT INTO fk_child (parent_id) VALUES (999)`)
	require.Error(t, fkErr)

	require.True(t, IsForeignKeyConstraintError(fkErr))
	require.False(t, IsUniqueConstraintError(fkErr))

	require.False(t, IsForeignKeyConstraintError(nil))
	require.False(t, IsForeignKeyConstraintError(errors.New("boom")))
}
