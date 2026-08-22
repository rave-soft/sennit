package db

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

// TestMigration_AddModelToSessions covers the part of the migration that
// can silently do nothing if it is wrong: the backfill. The columns
// themselves either exist or the migration fails loudly, but an UPDATE
// whose correlated subquery matches nothing leaves every row at the empty
// sentinel and still reports success — which reads exactly like "no
// session had a model to recover".
//
// Four sessions cover the cases the backfill has to tell apart: one with
// a clear history, one whose messages predate the provider column, one
// with no messages at all, and one whose newest message is the one that
// must win over an older, different model.
func TestMigration_AddModelToSessions(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "sennit.db")
	t.Cleanup(ResetPool)

	conn, err := openDB(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	require.NoError(t, initGoose())

	// Migrate to the last migration before the model columns existed, and
	// populate it the way a database of that vintage would be.
	require.NoError(t, goose.UpTo(conn, "migrations", 20260817000000))

	for _, id := range []string{"s-clear", "s-noprovider", "s-empty", "s-switched"} {
		_, err = conn.ExecContext(ctx, `
			INSERT INTO sessions (id, title, message_count, prompt_tokens,
				completion_tokens, cost, project_path, updated_at, created_at)
			VALUES (?, 'a session', 0, 0, 0, 0.0, '', 1000, 1000)`, id)
		require.NoError(t, err)
	}

	insertMsg := func(id, sessionID, model, provider string, createdAt int64) {
		t.Helper()
		_, err := conn.ExecContext(ctx, `
			INSERT INTO messages (id, session_id, role, parts, model, provider,
				created_at, updated_at)
			VALUES (?, ?, 'assistant', '[]', ?, ?, ?, ?)`,
			id, sessionID, model, provider, createdAt, createdAt)
		require.NoError(t, err)
	}

	insertMsg("m-1", "s-clear", "claude-opus-5", "anthropic", 1000)
	// A message from before the provider column was populated: a model id
	// with nothing to attribute it to.
	insertMsg("m-2", "s-noprovider", "some-model", "", 1000)
	// The newest message is the one that describes what the session runs
	// on now; the older one records a model it has since moved off.
	insertMsg("m-3", "s-switched", "old-model", "openai", 1000)
	insertMsg("m-4", "s-switched", "claude-sonnet-5", "anthropic", 2000)

	require.NoError(t, goose.Up(conn, "migrations"))

	readModel := func(sessionID string) (provider, model string) {
		t.Helper()
		require.NoError(t, conn.QueryRowContext(ctx,
			`SELECT model_provider, model_id FROM sessions WHERE id = ?`, sessionID,
		).Scan(&provider, &model))
		return provider, model
	}

	provider, model := readModel("s-clear")
	require.Equal(t, "anthropic", provider)
	require.Equal(t, "claude-opus-5", model)

	provider, model = readModel("s-switched")
	require.Equal(t, "anthropic", provider, "the newest message wins over an older, different model")
	require.Equal(t, "claude-sonnet-5", model)

	provider, model = readModel("s-noprovider")
	require.Empty(t, provider, "a model that cannot be attributed to a provider is not a usable pin")
	require.Empty(t, model)

	provider, model = readModel("s-empty")
	require.Empty(t, provider, "a session that never ran has no model to recover")
	require.Empty(t, model)

	// Down actually removes both columns rather than merely declaring it.
	require.NoError(t, goose.DownTo(conn, "migrations", 20260817000000))
	_, err = conn.ExecContext(ctx, `SELECT model_provider FROM sessions LIMIT 1`)
	require.Error(t, err, "model_provider must be gone after Down")
	_, err = conn.ExecContext(ctx, `SELECT model_id FROM sessions LIMIT 1`)
	require.Error(t, err, "model_id must be gone after Down")
}
