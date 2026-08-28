package appws

import (
	"testing"

	"github.com/google/uuid"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/message"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/stretchr/testify/require"
)

func TestPersistShellOutput_SkipsMissingSession(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	messages := messagestore.NewService(db.New(conn))

	missingID := uuid.New().String()
	err = persistShellOutput(t.Context(), messages, missingID, "cat file.txt", "hello", 0)
	require.NoError(t, err)

	stored, err := messages.List(t.Context(), missingID)
	require.NoError(t, err)
	require.Empty(t, stored)
}

func TestPersistShellOutput_NoOpForEmptySessionID(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	messages := messagestore.NewService(db.New(conn))

	require.NoError(t, persistShellOutput(t.Context(), messages, "", "echo hi", "hi", 0))
}

func TestPersistShellOutput_PersistsForExistingSession(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	q := db.New(conn)
	sessions := sessionstore.NewService(q, conn, "/test/project")
	messages := messagestore.NewService(q)

	sess, err := sessions.Create(t.Context(), "shell test")
	require.NoError(t, err)

	err = persistShellOutput(t.Context(), messages, sess.ID, "cat file.txt", "hello", 0)
	require.NoError(t, err)

	stored, err := messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Equal(t, message.User, stored[0].Role)
	shellParts := stored[0].ShellCommands()
	require.Len(t, shellParts, 1)
	require.Equal(t, "cat file.txt", shellParts[0].Command)
	require.Equal(t, "hello", shellParts[0].Output)
	require.Zero(t, shellParts[0].ExitCode)
}
