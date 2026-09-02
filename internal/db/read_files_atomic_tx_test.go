package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A Queries bound to a *sql.Tx cannot open a transaction of its own, and
// UpdateFileRead used to reject that with sql.ErrConnDone — "connection is
// already closed", which is neither true nor something a caller can act
// on. The caller's transaction already provides the atomicity the
// read-modify-write needs, so the work simply runs on it.
func TestUpdateFileRead_WorksOnATxBoundQueries(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	conn := newMigratedTestDB(t)

	_, err := conn.ExecContext(ctx, `INSERT INTO sessions (id, title, message_count, prompt_tokens, completion_tokens, cost, created_at, updated_at) VALUES ('s1', 't', 0, 0, 0, 0, 0, 0)`)
	require.NoError(t, err)

	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck

	q := New(tx)
	require.NoError(t, q.UpdateFileRead(ctx, "s1", "a.go", func(string) string { return "1-5" }),
		"a tx-bound Queries must use the caller's transaction, not report a closed connection")

	// Still uncommitted: the caller owns the commit.
	require.NoError(t, tx.Commit())

	var ranges string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT read_ranges FROM read_files WHERE session_id = ? AND path = ?`, "s1", "a.go").Scan(&ranges))
	require.Equal(t, "1-5", ranges)
}

// The read-modify-write still sees what a previous call wrote, whichever
// handle is providing the atomicity.
func TestUpdateFileRead_ReadModifyWriteAccumulates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	conn := newMigratedTestDB(t)

	_, err := conn.ExecContext(ctx, `INSERT INTO sessions (id, title, message_count, prompt_tokens, completion_tokens, cost, created_at, updated_at) VALUES ('s1', 't', 0, 0, 0, 0, 0, 0)`)
	require.NoError(t, err)

	q := New(conn)
	require.NoError(t, q.UpdateFileRead(ctx, "s1", "a.go", func(prev string) string {
		require.Empty(t, prev, "no row yet")
		return "1-5"
	}))
	require.NoError(t, q.UpdateFileRead(ctx, "s1", "a.go", func(prev string) string {
		require.Equal(t, "1-5", prev, "the second call must observe the first")
		return prev + ",9-12"
	}))

	var ranges string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT read_ranges FROM read_files WHERE session_id = ? AND path = ?`, "s1", "a.go").Scan(&ranges))
	require.Equal(t, "1-5,9-12", ranges)
}

// UpdateFileRead now delegates to the generated GetFileRead/RecordFileRead
// queries instead of hand-written SQL; the conflict target is (path,
// session_id), so two paths under the same session must not clobber each
// other's ranges.
func TestUpdateFileRead_DistinctPathsDoNotClobber(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	conn := newMigratedTestDB(t)

	_, err := conn.ExecContext(ctx, `INSERT INTO sessions (id, title, message_count, prompt_tokens, completion_tokens, cost, created_at, updated_at) VALUES ('s1', 't', 0, 0, 0, 0, 0, 0)`)
	require.NoError(t, err)

	q := New(conn)
	require.NoError(t, q.UpdateFileRead(ctx, "s1", "a.go", func(string) string { return "1-5" }))
	require.NoError(t, q.UpdateFileRead(ctx, "s1", "b.go", func(prev string) string {
		require.Empty(t, prev, "b.go must not see a.go's ranges")
		return "9-12"
	}))

	var ranges string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT read_ranges FROM read_files WHERE session_id = ? AND path = ?`, "s1", "a.go").Scan(&ranges))
	require.Equal(t, "1-5", ranges, "a.go's ranges must be unaffected by b.go's write")
}

func newMigratedTestDB(t *testing.T) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.db")
	conn, err := openDB(path)
	require.NoError(t, err)
	t.Cleanup(func() {
		conn.Close()
		_ = os.Remove(path)
	})
	conn.SetMaxOpenConns(1)
	require.NoError(t, migrate(conn))
	return conn
}
