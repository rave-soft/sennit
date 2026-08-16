package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// seedLegacyProjectDB creates a per-project legacy database (as it would
// have existed before every project moved to one shared database) with
// one session, one message, one file, one read file, and one thread.
func seedLegacyProjectDB(t *testing.T, projectDir string) {
	t.Helper()
	ctx := context.Background()

	conn, err := Connect(ctx, projectDir)
	require.NoError(t, err)
	defer func() { require.NoError(t, Release(projectDir)) }()

	q := New(conn)

	_, err = q.CreateSession(ctx, CreateSessionParams{
		ID:    "legacy-session-1",
		Title: "legacy session",
	})
	require.NoError(t, err)

	_, err = q.CreateMessage(ctx, CreateMessageParams{
		ID:        "legacy-message-1",
		SessionID: "legacy-session-1",
		Role:      "user",
		Parts:     "[]",
	})
	require.NoError(t, err)

	_, err = q.CreateFile(ctx, CreateFileParams{
		ID:        "legacy-file-1",
		SessionID: "legacy-session-1",
		Path:      "foo.go",
		Content:   "package foo",
	})
	require.NoError(t, err)

	_, err = conn.ExecContext(ctx, `INSERT INTO read_files (session_id, path, read_at) VALUES (?, ?, ?)`, "legacy-session-1", "foo.go", 100)
	require.NoError(t, err)

	_, err = q.CreateThread(ctx, CreateThreadParams{
		ID:           "legacy-thread-1",
		Name:         "legacy-thread",
		Goal:         "do the thing",
		BaseBranch:   "main",
		Branch:       "thread/legacy-thread",
		WorktreePath: "/tmp/legacy-thread",
		SessionID:    "legacy-session-1",
		Status:       "running",
	})
	require.NoError(t, err)

	_, err = conn.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, 100, "legacy-session-1")
	require.NoError(t, err)
}

func TestImportLegacyProjectDB(t *testing.T) {
	t.Cleanup(ResetPool)
	ctx := context.Background()

	projectDir := t.TempDir()
	globalDir := t.TempDir()
	projectPath := "/home/user/myproject"

	seedLegacyProjectDB(t, projectDir)

	legacyPath := filepath.Join(projectDir, "sennit.db")
	require.FileExists(t, legacyPath)

	dest, err := Connect(ctx, globalDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Release(globalDir) })

	err = ImportLegacyProjectDB(ctx, projectDir, projectPath, dest)
	require.NoError(t, err)

	// The legacy file must be renamed, not left in place.
	require.NoFileExists(t, legacyPath)
	require.FileExists(t, legacyPath+".imported")

	destQ := New(dest)

	sess, err := destQ.GetSessionByID(ctx, "legacy-session-1")
	require.NoError(t, err)
	require.Equal(t, projectPath, sess.ProjectPath)
	require.Equal(t, "legacy session", sess.Title)
	require.EqualValues(t, 1, sess.MessageCount)
	require.EqualValues(t, 100, sess.UpdatedAt)

	msgs, err := destQ.ListMessagesBySession(ctx, "legacy-session-1")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "legacy-message-1", msgs[0].ID)

	files, err := destQ.ListFilesBySession(ctx, "legacy-session-1")
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "legacy-file-1", files[0].ID)

	thread, err := destQ.GetThread(ctx, "legacy-thread-1")
	require.NoError(t, err)
	require.Equal(t, projectPath, thread.ProjectPath)
	require.Equal(t, "legacy-thread", thread.Name)

	_, err = dest.ExecContext(ctx, `UPDATE sessions SET title = ? WHERE id = ?`, "updated", "legacy-session-1")
	require.NoError(t, err)
	sess, err = destQ.GetSessionByID(ctx, "legacy-session-1")
	require.NoError(t, err)
	require.Greater(t, sess.UpdatedAt, int64(100))

	// Second call must be a no-op: the legacy file is gone, so nothing to
	// import, and no duplicate rows should appear.
	err = ImportLegacyProjectDB(ctx, projectDir, projectPath, dest)
	require.NoError(t, err)

	msgs, err = destQ.ListMessagesBySession(ctx, "legacy-session-1")
	require.NoError(t, err)
	require.Len(t, msgs, 1, "re-running the import must not duplicate rows")
}

func TestImportLegacyRows_SkipsDependentsOfConflictingSession(t *testing.T) {
	t.Cleanup(ResetPool)
	ctx := context.Background()
	legacyDir := t.TempDir()
	destDir := t.TempDir()
	seedLegacyProjectDB(t, legacyDir)

	legacy, err := Connect(ctx, legacyDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Release(legacyDir) })
	dest, err := Connect(ctx, destDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Release(destDir) })

	_, err = New(dest).CreateSession(ctx, CreateSessionParams{ID: "legacy-session-1", Title: "foreign session"})
	require.NoError(t, err)

	counts, err := importLegacyRows(ctx, legacy, dest, "/legacy-project")
	require.NoError(t, err)
	require.Equal(t, legacyImportCounts{
		sessionsSkipped:  1,
		messagesSkipped:  1,
		filesSkipped:     1,
		readFilesSkipped: 1,
		threadsSkipped:   1,
	}, counts)

	q := New(dest)
	session, err := q.GetSessionByID(ctx, "legacy-session-1")
	require.NoError(t, err)
	require.Equal(t, "foreign session", session.Title)
	_, err = q.GetMessage(ctx, "legacy-message-1")
	require.ErrorIs(t, err, sql.ErrNoRows)
	_, err = q.GetFile(ctx, "legacy-file-1")
	require.ErrorIs(t, err, sql.ErrNoRows)
	_, err = q.GetThread(ctx, "legacy-thread-1")
	require.ErrorIs(t, err, sql.ErrNoRows)
	var readFiles int
	require.NoError(t, dest.QueryRowContext(ctx, `SELECT COUNT(*) FROM read_files WHERE session_id = ?`, "legacy-session-1").Scan(&readFiles))
	require.Zero(t, readFiles)
}

// An orphaned child row (session_id referencing no session — e.g. left by
// an old crash with foreign keys off) must be skipped like a conflict, not
// abort the import: aborting would rename nothing, so every startup would
// retry the import and fail identically forever, leaving the user's whole
// legacy history invisible.
func TestImportLegacyRows_SkipsOrphanedChildRows(t *testing.T) {
	t.Cleanup(ResetPool)
	ctx := context.Background()
	legacyDir := t.TempDir()
	destDir := t.TempDir()
	seedLegacyProjectDB(t, legacyDir)

	legacy, err := Connect(ctx, legacyDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Release(legacyDir) })
	_, err = legacy.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	_, err = legacy.ExecContext(ctx, `INSERT INTO messages (id, session_id, role, parts, created_at, updated_at, is_summary_message) VALUES (?, ?, ?, ?, ?, ?, ?)`, "bad-message", "missing-session", "user", "[]", 100, 100, 0)
	require.NoError(t, err)
	_, err = legacy.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
	require.NoError(t, err)

	dest, err := Connect(ctx, destDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Release(destDir) })

	counts, err := importLegacyRows(ctx, legacy, dest, "/legacy-project")
	require.NoError(t, err, "an orphan row must not abort the import")
	require.Equal(t, 1, counts.messagesImported)
	require.Equal(t, 1, counts.messagesSkipped, "the orphan message must be counted as skipped")

	destQ := New(dest)
	sess, err := destQ.GetSessionByID(ctx, "legacy-session-1")
	require.NoError(t, err, "the healthy session must survive the orphan")
	require.EqualValues(t, 1, sess.MessageCount)
	_, err = destQ.GetMessage(ctx, "bad-message")
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// A session with no messages never has its updated_at bumped by the
// message-count trigger cascade, so the restore pass sees an unchanged
// value. It must skip such rows: an identity UPDATE matches the
// updated_at trigger's WHEN NEW = OLD clause and would rewrite the
// legacy timestamp to now — floating a years-old empty session to the
// top of the session list and past gc retention.
func TestImportLegacyRows_PreservesUpdatedAtOfEmptySession(t *testing.T) {
	t.Cleanup(ResetPool)
	ctx := context.Background()
	legacyDir := t.TempDir()
	destDir := t.TempDir()

	legacy, err := Connect(ctx, legacyDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Release(legacyDir) })
	q := New(legacy)
	_, err = q.CreateSession(ctx, CreateSessionParams{ID: "empty-session", Title: "no messages"})
	require.NoError(t, err)
	_, err = legacy.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, 100, "empty-session")
	require.NoError(t, err)

	dest, err := Connect(ctx, destDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Release(destDir) })

	_, err = importLegacyRows(ctx, legacy, dest, "/legacy-project")
	require.NoError(t, err)

	sess, err := New(dest).GetSessionByID(ctx, "empty-session")
	require.NoError(t, err)
	require.EqualValues(t, 0, sess.MessageCount)
	require.EqualValues(t, 100, sess.UpdatedAt, "empty session must keep its legacy updated_at")
}

func TestImportLegacyRows_SkipsUniqueChildConflict(t *testing.T) {
	t.Cleanup(ResetPool)
	ctx := context.Background()
	legacyDir := t.TempDir()
	destDir := t.TempDir()
	seedLegacyProjectDB(t, legacyDir)

	legacy, err := Connect(ctx, legacyDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Release(legacyDir) })
	dest, err := Connect(ctx, destDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Release(destDir) })
	q := New(dest)
	_, err = q.CreateSession(ctx, CreateSessionParams{ID: "other-session", Title: "other"})
	require.NoError(t, err)
	_, err = q.CreateMessage(ctx, CreateMessageParams{ID: "legacy-message-1", SessionID: "other-session", Role: "user", Parts: "[]"})
	require.NoError(t, err)

	counts, err := importLegacyRows(ctx, legacy, dest, "/legacy-project")
	require.NoError(t, err)
	require.EqualValues(t, 1, counts.sessionsImported)
	require.EqualValues(t, 1, counts.messagesSkipped)
	session, err := q.GetSessionByID(ctx, "legacy-session-1")
	require.NoError(t, err)
	require.Zero(t, session.MessageCount)
}

func TestImportLegacyProjectDB_NoLegacyFile(t *testing.T) {
	t.Cleanup(ResetPool)
	ctx := context.Background()

	projectDir := t.TempDir()
	globalDir := t.TempDir()

	dest, err := Connect(ctx, globalDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Release(globalDir) })

	err = ImportLegacyProjectDB(ctx, projectDir, "/some/project", dest)
	require.NoError(t, err)

	// No legacy file should have been created as a side effect.
	_, statErr := os.Stat(filepath.Join(projectDir, "sennit.db"))
	require.True(t, os.IsNotExist(statErr))
}

func TestImportLegacyRows_RollbacksOnOperationalError(t *testing.T) {
	t.Cleanup(ResetPool)
	ctx := context.Background()
	legacyDir := t.TempDir()
	destDir := t.TempDir()

	seedLegacyProjectDB(t, legacyDir)

	legacy, err := Connect(ctx, legacyDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Release(legacyDir) })

	dest, err := Connect(ctx, destDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Release(destDir) })

	_, err = dest.ExecContext(ctx, `
		CREATE TRIGGER rollback_test_trigger
		BEFORE INSERT ON messages FOR EACH ROW
		BEGIN
			SELECT CASE WHEN NEW.id = 'legacy-message-1'
				THEN RAISE(ABORT, 'trigger blocks import') END;
		END;
	`)
	require.NoError(t, err)

	_, err = importLegacyRows(ctx, legacy, dest, "/legacy-project")
	require.Error(t, err)

	destQ := New(dest)

	_, err = destQ.GetSessionByID(ctx, "legacy-session-1")
	require.ErrorIs(t, err, sql.ErrNoRows)

	_, err = destQ.GetMessage(ctx, "legacy-message-1")
	require.ErrorIs(t, err, sql.ErrNoRows)

	var cnt int
	require.NoError(t, dest.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions`).Scan(&cnt))
	require.Zero(t, cnt)
	require.NoError(t, dest.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages`).Scan(&cnt))
	require.Zero(t, cnt)

	_, err = dest.ExecContext(ctx, `DROP TRIGGER IF EXISTS rollback_test_trigger`)
	require.NoError(t, err)
}
