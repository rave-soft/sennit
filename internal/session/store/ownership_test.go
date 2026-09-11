package store

import (
	"testing"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/stretchr/testify/require"
)

func TestOwnershipStore_PrepareCommitFencesSource(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })

	// The ownership table deliberately references a real session row.
	sessions := NewService(db.New(conn), conn, dataDir)
	created, err := sessions.Create(t.Context(), "ownership")
	require.NoError(t, err)

	owners := NewOwnershipStore(conn)
	source, err := owners.Claim(t.Context(), created.ID, "main", "/repo")
	require.NoError(t, err)
	require.EqualValues(t, 1, source.Epoch)
	require.NoError(t, owners.Prepare(t.Context(), created.ID, "main", source.Epoch, Ownership{OwnerID: "worktree", OwnerRoot: "/repo/.sennit/worktrees/feature", WorktreeName: "feature", WorktreePath: "/repo/.sennit/worktrees/feature"}))

	stillSource, err := owners.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "main", stillSource.OwnerID)
	require.Equal(t, "preparing", stillSource.Phase)
	owned, err := owners.IsOwner(t.Context(), created.ID, "main", source.Epoch)
	require.NoError(t, err)
	require.True(t, owned, "preparing keeps the source epoch authoritative")

	target, err := owners.Commit(t.Context(), created.ID, "main", source.Epoch)
	require.NoError(t, err)
	require.Equal(t, "worktree", target.OwnerID)
	require.EqualValues(t, 2, target.Epoch)
	owned, err = owners.IsOwner(t.Context(), created.ID, "main", source.Epoch)
	require.NoError(t, err)
	require.False(t, owned)
	owned, err = owners.IsOwner(t.Context(), created.ID, "worktree", target.Epoch)
	require.NoError(t, err)
	require.True(t, owned)
}

func TestOwnershipStore_RejectsStaleRollbackAndRecoversPreparing(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	sessions := NewService(db.New(conn), conn, dataDir)
	created, err := sessions.Create(t.Context(), "ownership")
	require.NoError(t, err)
	owners := NewOwnershipStore(conn)
	source, err := owners.Claim(t.Context(), created.ID, "main", "/repo")
	require.NoError(t, err)
	require.ErrorIs(t, owners.Rollback(t.Context(), created.ID, "main", source.Epoch), ErrInvalidPhase)
	require.NoError(t, owners.Prepare(t.Context(), created.ID, "main", source.Epoch, Ownership{OwnerID: "target", OwnerRoot: "/target"}))
	recovered, err := owners.Recover(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "main", recovered.OwnerID)
	require.Equal(t, "stable", recovered.Phase)
	require.Equal(t, source.Epoch, recovered.Epoch)
}

func TestOwnershipStore_AdoptFencesDeadOwnerAtStableLocation(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	sessions := NewService(db.New(conn), conn, dataDir)
	created, err := sessions.Create(t.Context(), "ownership")
	require.NoError(t, err)
	owners := NewOwnershipStore(conn)
	first, err := owners.Claim(t.Context(), created.ID, "dead", "/repo")
	require.NoError(t, err)
	adopted, err := owners.Adopt(t.Context(), created.ID, "restarted", "/repo")
	require.NoError(t, err)
	require.Equal(t, first.Epoch+1, adopted.Epoch)
	require.Equal(t, "restarted", adopted.OwnerID)
	_, err = owners.Adopt(t.Context(), created.ID, "foreign", "/other")
	require.ErrorIs(t, err, ErrNotOwner)
}

func TestOwnershipStore_AdoptRecoversPreparingToSourceLocation(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	sessions := NewService(db.New(conn), conn, dataDir)
	created, err := sessions.Create(t.Context(), "ownership")
	require.NoError(t, err)
	owners := NewOwnershipStore(conn)
	first, err := owners.Claim(t.Context(), created.ID, "dead", "/repo")
	require.NoError(t, err)
	require.NoError(t, owners.Prepare(t.Context(), created.ID, first.OwnerID, first.Epoch, Ownership{OwnerID: "target", OwnerRoot: "/worktree"}))
	adopted, err := owners.Adopt(t.Context(), created.ID, "restarted", "/repo")
	require.NoError(t, err)
	require.Equal(t, "stable", adopted.Phase)
	require.Equal(t, "restarted", adopted.OwnerID)
	require.Equal(t, first.Epoch+1, adopted.Epoch)
}

func TestOwnershipStore_RollbackLeavesSourceOwner(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	sessions := NewService(db.New(conn), conn, dataDir)
	created, err := sessions.Create(t.Context(), "ownership")
	require.NoError(t, err)
	owners := NewOwnershipStore(conn)
	source, err := owners.Claim(t.Context(), created.ID, "main", "/repo")
	require.NoError(t, err)
	require.NoError(t, owners.Prepare(t.Context(), created.ID, "main", source.Epoch, Ownership{OwnerID: "target", OwnerRoot: "/target"}))
	require.NoError(t, owners.Rollback(t.Context(), created.ID, "main", source.Epoch))
	got, err := owners.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "main", got.OwnerID)
	require.Equal(t, "stable", got.Phase)
}
