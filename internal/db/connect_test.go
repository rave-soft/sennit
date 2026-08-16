package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConnect_SharesConnectionForSameDataDir(t *testing.T) {
	t.Cleanup(ResetPool)

	dataDir := t.TempDir()

	conn1, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)

	conn2, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)

	require.Same(t, conn1, conn2, "should return the same *sql.DB for the same data dir")

	// Releasing once should not close the connection.
	require.NoError(t, Release(dataDir))
	require.NoError(t, conn1.PingContext(context.Background()), "connection should still be usable after partial release")

	// Releasing again should close it.
	require.NoError(t, Release(dataDir))
	require.Error(t, conn1.PingContext(context.Background()), "connection should be closed after final release")
}

func TestConnect_SeparateConnectionsForDifferentDataDirs(t *testing.T) {
	t.Cleanup(ResetPool)

	dir1 := t.TempDir()
	dir2 := t.TempDir()

	conn1, err := Connect(context.Background(), dir1)
	require.NoError(t, err)

	conn2, err := Connect(context.Background(), dir2)
	require.NoError(t, err)

	require.NotSame(t, conn1, conn2, "different data dirs should get different connections")

	require.NoError(t, Release(dir1))
	require.NoError(t, Release(dir2))
}

func TestRelease_NoopForUnknownDataDir(t *testing.T) {
	t.Cleanup(ResetPool)

	require.NoError(t, Release("/nonexistent/path"), "releasing unknown data dir should not error")
}

// TestConnect_IgnoresContendedWorkspaceLock confirms Connect no longer
// takes or checks the workspace lock at all: two "processes" can both
// hold a lock.TryFile on the directory's sennit.lock and Connect still
// succeeds. Locking is now a separate, opt-in concern handled by
// AcquireWorkspaceLock, since a single shared database is opened by
// many concurrent, legitimately-unrelated project workspaces.
func TestConnect_IgnoresContendedWorkspaceLock(t *testing.T) {
	t.Cleanup(ResetPool)

	dataDir := t.TempDir()

	lockObj, err := AcquireWorkspaceLock(dataDir)
	require.NoError(t, err, "expected to take the workspace lock for the first time")
	t.Cleanup(lockObj.Release)

	conn, err := Connect(context.Background(), dataDir)
	require.NoError(t, err, "Connect must not take the lock and must succeed under contention")
	require.NoError(t, conn.PingContext(context.Background()))
	require.NoError(t, Release(dataDir))
}
