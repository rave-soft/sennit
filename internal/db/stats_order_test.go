package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProjectStatsSince_OrdersByTotalNotPerRowColumn pins that
// ProjectStatsSince orders projects by the SUMmed token total, not by
// prompt_tokens/completion_tokens read off an arbitrary row of the group.
// SQLite only resolves an output alias for a bare identifier; inside an
// expression like "(prompt_tokens + completion_tokens)" the names bind to
// the source columns of an arbitrary row in the group, not the aggregate.
//
// Project A gets two sessions (1 and 100 prompt tokens, inserted in that
// order — SUM 101); project B gets one session (50 prompt tokens). Before
// the fix, SQLite evaluated the ORDER BY expression against the first row
// of A's group (1), so B (50) sorted above A (101). The fix orders by the
// same COALESCE(SUM(...)) expression the SELECT list already computes.
func TestProjectStatsSince_OrdersByTotalNotPerRowColumn(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(ResetPool)

	conn, err := Connect(t.Context(), dataDir)
	require.NoError(t, err)
	q := New(conn)
	ctx := t.Context()

	_, err = q.CreateSession(ctx, CreateSessionParams{
		ID: "a1", Title: "a1", ProjectPath: "/proj/a", PromptTokens: 1,
	})
	require.NoError(t, err)
	_, err = q.CreateSession(ctx, CreateSessionParams{
		ID: "a2", Title: "a2", ProjectPath: "/proj/a", PromptTokens: 100,
	})
	require.NoError(t, err)
	_, err = q.CreateSession(ctx, CreateSessionParams{
		ID: "b1", Title: "b1", ProjectPath: "/proj/b", PromptTokens: 50,
	})
	require.NoError(t, err)

	rows, err := q.ProjectStatsSince(ctx, 0)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	require.Equal(t, "/proj/a", rows[0].ProjectPath, "project with the higher SUMmed total (101) must rank first, not the group's arbitrary per-row value")
	require.Equal(t, "/proj/b", rows[1].ProjectPath)
}
