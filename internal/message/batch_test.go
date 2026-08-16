package message

import (
	"context"
	"errors"
	"testing"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/stretchr/testify/require"
)

type failingBatchQuerier struct {
	db.Querier
	err error
}

func (q failingBatchQuerier) ListMessagesBySessionIDs(context.Context, string) ([]db.Message, error) {
	return nil, q.err
}

func TestListBySessionIDs(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn, "/test/project")
	root, err := sessions.Create(t.Context(), "root")
	require.NoError(t, err)
	child, err := sessions.CreateTaskSession(t.Context(), "child", root.ID, "child")
	require.NoError(t, err)
	sibling, err := sessions.CreateTaskSession(t.Context(), "sibling", root.ID, "sibling")
	require.NoError(t, err)

	svc := NewService(q)
	for _, tc := range []struct {
		sessionID string
		text      string
	}{
		{root.ID, "root"},
		{child.ID, "child"},
		{sibling.ID, "sibling"},
	} {
		_, err := svc.Create(t.Context(), tc.sessionID, CreateMessageParams{
			Role:  User,
			Parts: []ContentPart{TextContent{Text: tc.text}},
		})
		require.NoError(t, err)
	}

	got, err := svc.ListBySessionIDs(t.Context(), []string{child.ID, root.ID})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "root", got[root.ID][0].Content().Text)
	require.Equal(t, "child", got[child.ID][0].Content().Text)
	require.NotContains(t, got, sibling.ID)
}

func TestListBySessionIDsPropagatesQueryError(t *testing.T) {
	t.Parallel()

	errExpected := errors.New("batch query failed")
	svc := NewService(failingBatchQuerier{err: errExpected})
	_, err := svc.ListBySessionIDs(t.Context(), []string{"session"})
	require.ErrorIs(t, err, errExpected)
}

func TestListBySessionIDsSkipsCorruptMessage(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn, "/test/project")
	root, err := sessions.Create(t.Context(), "root")
	require.NoError(t, err)
	svc := NewService(q)
	valid, err := svc.Create(t.Context(), root.ID, CreateMessageParams{Role: User, Parts: []ContentPart{TextContent{Text: "valid"}}})
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "UPDATE messages SET parts = ? WHERE id = ?", "{", valid.ID)
	require.NoError(t, err)
	other, err := svc.Create(t.Context(), root.ID, CreateMessageParams{Role: User, Parts: []ContentPart{TextContent{Text: "sibling"}}})
	require.NoError(t, err)

	got, err := svc.ListBySessionIDs(t.Context(), []string{root.ID})
	require.NoError(t, err)
	require.Len(t, got[root.ID], 1)
	require.Equal(t, other.ID, got[root.ID][0].ID)
}
