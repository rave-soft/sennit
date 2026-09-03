package db

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

// TestMigration_ReadAtMilliseconds verifies that a pre-existing read_files
// row, recorded with second-resolution read_at, comes out scaled to
// milliseconds after the migration runs — and that Down reverses it.
func TestMigration_ReadAtMilliseconds(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(ResetPool)
	dbPath := filepath.Join(dataDir, "sennit.db")

	conn, err := openDB(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	require.NoError(t, initGoose())

	// Migrate up to the last migration before this one, and insert a row
	// the way a pre-migration database would have it: read_at in seconds.
	require.NoError(t, goose.UpTo(conn, "migrations", 20260831000000))
	_, err = conn.ExecContext(context.Background(), `
		INSERT INTO sessions (id, title, updated_at, created_at)
		VALUES ('sess-1', 'a session', 1000, 1000)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), `
		INSERT INTO read_files (session_id, path, read_at)
		VALUES ('sess-1', 'main.go', 1700000000)`)
	require.NoError(t, err)

	require.NoError(t, goose.Up(conn, "migrations"))

	var readAt int64
	err = conn.QueryRowContext(context.Background(),
		`SELECT read_at FROM read_files WHERE session_id = 'sess-1' AND path = 'main.go'`).
		Scan(&readAt)
	require.NoError(t, err)
	require.Equal(t, int64(1700000000000), readAt, "seconds must be scaled to milliseconds")

	require.NoError(t, goose.DownTo(conn, "migrations", 20260831000000))
	err = conn.QueryRowContext(context.Background(),
		`SELECT read_at FROM read_files WHERE session_id = 'sess-1' AND path = 'main.go'`).
		Scan(&readAt)
	require.NoError(t, err)
	require.Equal(t, int64(1700000000), readAt, "Down should divide back to seconds")
}
