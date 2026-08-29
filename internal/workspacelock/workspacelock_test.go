package db

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/lock"
	"github.com/stretchr/testify/require"
)

// TestAcquireWorkspaceLock_FailsWhenContended simulates a second sennit
// process by taking the workspace lock directly via the OS primitive on
// a separate file descriptor and then asserting that
// AcquireWorkspaceLock surfaces a clean ErrWorkspaceLocked instead of
// acquiring under contention.
func TestAcquireWorkspaceLock_FailsWhenContended(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, workspaceLockFile)

	release, err := lock.TryFile(lockPath)
	require.NoError(t, err, "expected to take the workspace lock for the first time")
	t.Cleanup(release)

	_, err = AcquireWorkspaceLock(dir)
	require.Error(t, err, "AcquireWorkspaceLock must refuse a contended directory")
	require.ErrorIs(t, err, ErrWorkspaceLocked)
}

// TestAcquireWorkspaceLock_SucceedsAfterContenderReleases ensures the
// lock is purely advisory and that a clean release lets the next
// acquisition proceed.
func TestAcquireWorkspaceLock_SucceedsAfterContenderReleases(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, workspaceLockFile)

	release, err := lock.TryFile(lockPath)
	require.NoError(t, err)

	_, err = AcquireWorkspaceLock(dir)
	require.ErrorIs(t, err, ErrWorkspaceLocked)

	release()

	l, err := AcquireWorkspaceLock(dir)
	require.NoError(t, err, "should succeed once the contender releases the lock")
	l.Release()
}

// TestAcquireWorkspaceLock_ReleaseFreesLock confirms Release drops the
// OS lock so a subsequent acquirer succeeds.
func TestAcquireWorkspaceLock_ReleaseFreesLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, workspaceLockFile)

	l, err := AcquireWorkspaceLock(dir)
	require.NoError(t, err)

	_, lockErr := lock.TryFile(lockPath)
	require.Error(t, lockErr)
	require.True(t, errors.Is(lockErr, lock.ErrContended), "expected contended lock while held")

	l.Release()

	release, err := lock.TryFile(lockPath)
	require.NoError(t, err, "expected lock to be released")
	release()
}

// TestAcquireWorkspaceLock_SkipEnvBypassesAcquisition exercises the
// escape hatch used by users on filesystems where flock is unreliable.
func TestAcquireWorkspaceLock_SkipEnvBypassesAcquisition(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, workspaceLockFile)

	release, err := lock.TryFile(lockPath)
	require.NoError(t, err)
	t.Cleanup(release)

	t.Setenv("SENNIT_SKIP_DATADIR_LOCK", "1")

	l, err := AcquireWorkspaceLock(dir)
	require.NoError(t, err, "skip-lock env should bypass contention")
	l.Release()
}

// TestWorkspaceLock_ReleaseNilSafe confirms calling Release on a nil
// *WorkspaceLock (the value callers hold when locking wasn't requested)
// is a no-op rather than a panic.
func TestWorkspaceLock_ReleaseNilSafe(t *testing.T) {
	var l *WorkspaceLock
	l.Release()
}
