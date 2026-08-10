package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// seedLegacyProjectDB creates a per-project legacy database (as it would
// have existed before every project moved to one shared database) with
// one session, one message, one file, and one thread.
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

	_, err = q.CreateThread(ctx, CreateThreadParams{
		ID:           "legacy-thread-1",
		Name:         "legacy-thread",
		Goal:         "do the thing",
		BaseBranch:   "main",
		Branch:       "thread/legacy-thread",
		WorktreePath: "/tmp/legacy-thread",
		Status:       "running",
	})
	require.NoError(t, err)
}

func TestImportLegacyProjectDB(t *testing.T) {
	t.Cleanup(ResetPool)
	ctx := context.Background()

	projectDir := t.TempDir()
	globalDir := t.TempDir()
	projectPath := "/home/user/myproject"

	seedLegacyProjectDB(t, projectDir)

	legacyPath := filepath.Join(projectDir, "braid.db")
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

	// Second call must be a no-op: the legacy file is gone, so nothing to
	// import, and no duplicate rows should appear.
	err = ImportLegacyProjectDB(ctx, projectDir, projectPath, dest)
	require.NoError(t, err)

	msgs, err = destQ.ListMessagesBySession(ctx, "legacy-session-1")
	require.NoError(t, err)
	require.Len(t, msgs, 1, "re-running the import must not duplicate rows")
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
	_, statErr := os.Stat(filepath.Join(projectDir, "braid.db"))
	require.True(t, os.IsNotExist(statErr))
}
