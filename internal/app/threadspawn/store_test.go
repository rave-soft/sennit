package threadspawn

import (
	"context"
	"errors"
	"testing"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

// TestStore_CreateReturnsErrNameTakenOnDuplicateName pins the real
// store's half of the thread.Store contract documented on
// thread.ErrNameTaken: Create must report a (project_path, kind, name)
// collision as an error wrapping ErrNameTaken, not a raw driver error,
// so thread.Manager's check-then-act race guard (see Manager.Create)
// keeps working against this implementation.
func TestStore_CreateReturnsErrNameTakenOnDuplicateName(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })

	store := NewStore(db.New(conn), dataDir)

	_, err = store.Create(context.Background(), thread.CreateParams{
		Name:         "dup",
		Branch:       "thread/dup",
		WorktreePath: t.TempDir(),
	})
	require.NoError(t, err)

	_, err = store.Create(context.Background(), thread.CreateParams{
		Name:         "dup",
		Branch:       "thread/dup-2",
		WorktreePath: t.TempDir(),
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, thread.ErrNameTaken))
}
