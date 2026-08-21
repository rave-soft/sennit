package db

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rave-soft/sennit/internal/brand"
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

func TestRelease_UnknownDataDirIsReportedMisuse(t *testing.T) {
	t.Cleanup(ResetPool)

	require.Error(t, Release("/nonexistent/path"),
		"releasing a data dir with no Connect behind it is misuse, not a no-op")
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

// TestRelease_ClosesHandleWhenRefcountHitsZeroEvenForNonReleasingHolder
// pins the property behind the hazard documented in TECHDEBT.md ("An
// over-release closes the shared DB connection out from under other
// holders"): a *sql.DB returned by Connect is closed as soon as the
// pool's refcount for that data dir reaches zero, regardless of which
// goroutine's Release call brought it there. conn1 below never calls
// Release itself — conn2's two Releases are what drain the count — yet
// conn1's handle dies at the same moment conn2's does, because both are
// the same *sql.DB and the pool tracks one shared count, not per-holder
// permission to use the handle.
//
// The pool's inputs are just a data dir and a count, so it has no way to
// tell "the same holder released twice" from "two different holders
// each released once": both are indistinguishable transitions of
// 2 -> 1 -> 0. That is exactly why the TECHDEBT.md entry calls an
// over-release undetectable — this test pins the closing-at-zero
// consequence, not the unbalanced call itself, which leaves no trace to
// pin.
func TestRelease_ClosesHandleWhenRefcountHitsZeroEvenForNonReleasingHolder(t *testing.T) {
	t.Cleanup(ResetPool)

	dataDir := t.TempDir()

	// Two Connects share one handle and bring refCount to 2. conn1 is the
	// non-releasing holder in this scenario; conn2 is not queried again,
	// it exists only so its two Releases below are the ones draining the
	// count.
	conn1, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)
	conn2, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)
	require.Same(t, conn1, conn2)
	require.NoError(t, conn1.PingContext(context.Background()), "sanity: connection works before any release")

	// First release: refCount 2 -> 1. conn1 did not call this and is
	// still live.
	require.NoError(t, Release(dataDir))
	require.NoError(t, conn1.PingContext(context.Background()), "still usable after one release")

	// Second release: refCount 1 -> 0, so the pool closes the shared
	// handle and deletes the entry. conn1 still never released anything,
	// but its handle is the same *sql.DB conn2 was using, so it dies too.
	require.NoError(t, Release(dataDir))
	require.Error(t, conn1.PingContext(context.Background()), "handle closes at refcount zero regardless of which holder's Release drained it")

	// An extra release past zero finds no entry left to drain. It used to
	// be a silent no-op — exactly the "nothing detects it" half of the
	// TECHDEBT.md entry this whole file is pinning the fix for — and is
	// now reported misuse instead; see TestRelease_PastZeroIsReportedMisuse.
	require.Error(t, Release(dataDir), "release past zero is reported, not swallowed")
}

// TestRelease_PastZeroIsReportedMisuse covers Release called again after
// the pool entry has already been removed (as opposed to
// TestRelease_UnknownDataDirIsReportedMisuse, which covers a data dir that
// was never connected at all). Both used to return nil silently; both now
// surface as a reported (logged and returned) error instead of pretending
// the call was fine.
func TestRelease_PastZeroIsReportedMisuse(t *testing.T) {
	t.Cleanup(ResetPool)

	dataDir := t.TempDir()

	_, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)

	require.NoError(t, Release(dataDir), "balanced release removes the entry")
	require.Error(t, Release(dataDir), "release after the entry is already gone must be reported, not silently accepted")
}

// TestConnect_NormalizesPathSpelling pins that Connect and Release both
// resolve dataDir through filepath.Abs before indexing the pool, so two
// different spellings of the same directory share one entry instead of
// opening a second connection.
func TestConnect_NormalizesPathSpelling(t *testing.T) {
	t.Cleanup(ResetPool)

	dataDir := t.TempDir()

	cwd, err := filepath.Abs(".")
	require.NoError(t, err)
	relDir, err := filepath.Rel(cwd, dataDir)
	require.NoError(t, err)

	// A spelling with a trailing separator normalizes the same way.
	trailingDir := dataDir + string(filepath.Separator)

	conn1, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)

	conn2, err := Connect(context.Background(), relDir)
	require.NoError(t, err)
	require.Same(t, conn1, conn2, "relative and absolute spellings of the same dir should share one connection")

	conn3, err := Connect(context.Background(), trailingDir)
	require.NoError(t, err)
	require.Same(t, conn1, conn3, "a trailing separator should not open a second connection")

	// Three Connects, one entry: three Releases fully unwind it.
	require.NoError(t, Release(dataDir))
	require.NoError(t, Release(relDir))
	require.NoError(t, conn1.PingContext(context.Background()), "still usable after two of three releases")
	require.NoError(t, Release(trailingDir))
	require.Error(t, conn1.PingContext(context.Background()), "closed after the third, balanced release")
}

// TestConnect_ConcurrentAccess hammers Connect/Release for the same and
// different data dirs from multiple goroutines, to be run with -race.
// poolMu must serialize all access to the map with no leaked or
// prematurely closed connection when every Connect is paired with
// exactly one Release.
func TestConnect_ConcurrentAccess(t *testing.T) {
	t.Cleanup(ResetPool)

	const goroutinesPerDir = 5
	const itersPerGoroutine = 3

	dirs := []string{t.TempDir(), t.TempDir()}

	var wg sync.WaitGroup
	for _, dir := range dirs {
		for range goroutinesPerDir {
			wg.Add(1)
			go func(dataDir string) {
				defer wg.Done()
				for range itersPerGoroutine {
					conn, err := Connect(context.Background(), dataDir)
					require.NoError(t, err)
					require.NoError(t, conn.PingContext(context.Background()))
					require.NoError(t, Release(dataDir))
				}
			}(dir)
		}
	}
	wg.Wait()

	// Every Connect was paired with exactly one Release, so no entry
	// should remain in the pool for either data dir.
	poolMu.Lock()
	for _, dir := range dirs {
		absPath, err := filepath.Abs(filepath.Join(dir, brand.DBFile))
		require.NoError(t, err)
		_, ok := pool[absPath]
		require.False(t, ok, "balanced concurrent Connect/Release should leave no pool entry for %s", dir)
	}
	poolMu.Unlock()

	// A fresh Connect after the concurrent storm still works.
	conn, err := Connect(context.Background(), dirs[0])
	require.NoError(t, err)
	require.NoError(t, conn.PingContext(context.Background()))
	require.NoError(t, Release(dirs[0]))
}

// TestResetPool_ClosesAndClearsPool confirms ResetPool closes every
// pooled connection and empties the pool, and that a subsequent Connect
// starts fresh rather than reusing a closed handle.
func TestResetPool_ClosesAndClearsPool(t *testing.T) {
	t.Cleanup(ResetPool)

	dataDir := t.TempDir()

	conn1, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)

	ResetPool()
	require.Error(t, conn1.PingContext(context.Background()), "ResetPool must close the pooled connection")

	conn2, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)
	require.NotSame(t, conn1, conn2, "ResetPool should force a fresh connection on next Connect")
	require.NoError(t, conn2.PingContext(context.Background()))
	require.NoError(t, Release(dataDir))
}
