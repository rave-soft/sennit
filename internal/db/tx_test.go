package db

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInTx_CommitsOnSuccess(t *testing.T) {
	t.Cleanup(ResetPool)
	dataDir := t.TempDir()
	conn, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)

	err = InTx(context.Background(), conn, func(q *Queries) error {
		_, err := q.CreateSession(context.Background(), CreateSessionParams{ID: "s1", Title: "t", ProjectPath: "/p"})
		return err
	})
	require.NoError(t, err)

	q := New(conn)
	_, err = q.GetSessionByID(context.Background(), "s1")
	require.NoError(t, err, "row created inside fn must be visible after InTx commits")
}

func TestInTx_RollsBackOnError(t *testing.T) {
	t.Cleanup(ResetPool)
	dataDir := t.TempDir()
	conn, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)

	errInjected := errors.New("injected failure")
	err = InTx(context.Background(), conn, func(q *Queries) error {
		_, err := q.CreateSession(context.Background(), CreateSessionParams{ID: "s2", Title: "t", ProjectPath: "/p"})
		require.NoError(t, err)
		return errInjected
	})
	require.ErrorIs(t, err, errInjected)

	q := New(conn)
	_, err = q.GetSessionByID(context.Background(), "s2")
	require.Error(t, err, "row created inside fn must not survive a rolled-back InTx")
}
