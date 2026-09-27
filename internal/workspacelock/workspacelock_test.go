package workspacelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/lock"
	"github.com/stretchr/testify/require"
)

// TestAcquireWorkspaceLock_FailsWhenContended simulates a second sennit
// process by taking the workspace lock directly via the OS primitive on
// a separate file descriptor and then asserting that
// Acquire surfaces a clean ErrLocked instead of
// acquiring under contention.
func TestAcquireWorkspaceLock_FailsWhenContended(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, lockFileName)

	release, err := lock.TryFile(lockPath)
	require.NoError(t, err, "expected to take the workspace lock for the first time")
	t.Cleanup(release)

	_, err = Acquire(dir)
	require.Error(t, err, "Acquire must refuse a contended directory")
	require.ErrorIs(t, err, ErrLocked)
}

// TestAcquireWorkspaceLock_SucceedsAfterContenderReleases ensures the
// lock is purely advisory and that a clean release lets the next
// acquisition proceed.
func TestAcquireWorkspaceLock_SucceedsAfterContenderReleases(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, lockFileName)

	release, err := lock.TryFile(lockPath)
	require.NoError(t, err)

	_, err = Acquire(dir)
	require.ErrorIs(t, err, ErrLocked)

	release()

	l, err := Acquire(dir)
	require.NoError(t, err, "should succeed once the contender releases the lock")
	l.Release()
}

// TestAcquireWorkspaceLock_ReleaseFreesLock confirms Release drops the
// OS lock so a subsequent acquirer succeeds.
func TestAcquireWorkspaceLock_ReleaseFreesLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, lockFileName)

	l, err := Acquire(dir)
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
	lockPath := filepath.Join(dir, lockFileName)

	release, err := lock.TryFile(lockPath)
	require.NoError(t, err)
	t.Cleanup(release)

	t.Setenv("SENNIT_SKIP_DATADIR_LOCK", "1")

	l, err := Acquire(dir)
	require.NoError(t, err, "skip-lock env should bypass contention")
	l.Release()
}

// TestWorkspaceLock_ReleaseNilSafe confirms calling Release on a nil
// *Lock (the value callers hold when locking wasn't requested)
// is a no-op rather than a panic.
func TestWorkspaceLock_ReleaseNilSafe(t *testing.T) {
	var l *Lock
	l.Release()
}

// TestAcquire_DefaultModeIsTUI pins that a plain Acquire (no WithMode)
// records ModeTUI with no socket, so a reader never has to special-case
// "mode absent" for the common, non-daemon caller.
func TestAcquire_DefaultModeIsTUI(t *testing.T) {
	dir := t.TempDir()

	l, err := Acquire(dir)
	require.NoError(t, err)
	t.Cleanup(l.Release)

	owner, ok, err := CurrentOwner(dir)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, ModeTUI, owner.Mode)
	require.Empty(t, owner.Socket)
	require.Equal(t, os.Getpid(), owner.PID)
}

// TestAcquire_WithModeRecordsDaemonAndSocket pins that WithMode(ModeDaemon,
// socket) is what CurrentOwner reports back, round-tripping through the
// JSON lock file rather than only living in memory.
func TestAcquire_WithModeRecordsDaemonAndSocket(t *testing.T) {
	dir := t.TempDir()
	const socket = "/run/sennit/deadbeefdeadbeef.sock"

	l, err := Acquire(dir, WithMode(ModeDaemon, socket))
	require.NoError(t, err)
	t.Cleanup(l.Release)

	owner, ok, err := CurrentOwner(dir)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, ModeDaemon, owner.Mode)
	require.Equal(t, socket, owner.Socket)
}

// TestCurrentOwner_OldFormatRecordReadsAsTUI pins backward compatibility:
// an owner-info file written by a binary that predates Mode/Socket (just
// pid, version, started_at) must still be read, with Mode normalized to
// ModeTUI rather than left as the JSON-decoded zero value "".
func TestCurrentOwner_OldFormatRecordReadsAsTUI(t *testing.T) {
	dir := t.TempDir()
	ownerPath := filepath.Join(dir, ownerInfoFileName)
	require.NoError(t, os.WriteFile(ownerPath,
		[]byte(`{"pid": 4242, "version": "0.1.0", "started_at": "2020-01-01T00:00:00Z"}`), 0o600))

	owner, ok, err := CurrentOwner(dir)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, ModeTUI, owner.Mode)
	require.Equal(t, 4242, owner.PID)
	require.Empty(t, owner.Socket)
}

// TestCurrentOwner_ReadableWhileLockFileIsHeld is the regression test for
// the Windows failure this package fixed: owner info must be readable
// while the lock file itself is actively locked by someone else.
//
// A second, independent OS lock handle on the same lock path (bypassing
// Acquire's in-process pool) stands in for a second real process holding
// the workspace lock. On Windows, LockFileEx locks the whole file
// (lock_windows.go) and is mandatory: any other handle's ReadFile on
// that range fails outright, which is exactly what broke CurrentOwner
// when owner info lived inside the locked file. POSIX flock never
// exhibits this (a plain read is never blocked by another process's
// flock), so this test can only prove the fix holds on Linux/macOS by
// exercising the same code path with contention proven, not the
// Windows-specific mandatory-lock failure itself -- see this package's
// review notes for why that failure can't be reproduced on Linux.
func TestCurrentOwner_ReadableWhileLockFileIsHeld(t *testing.T) {
	dir := t.TempDir()

	l, err := Acquire(dir, WithMode(ModeDaemon, "/run/sennit/test.sock"))
	require.NoError(t, err)
	t.Cleanup(l.Release)

	// Prove a second handle sees the lock file as actively locked --
	// standing in for a second process holding it.
	lockPath := filepath.Join(dir, lockFileName)
	_, err = lock.TryFile(lockPath)
	require.ErrorIs(t, err, lock.ErrContended, "expected the lock file to be contended while Acquire holds it")

	owner, ok, err := CurrentOwner(dir)
	require.NoError(t, err, "owner info must be readable even while the lock file is held elsewhere")
	require.True(t, ok)
	require.Equal(t, ModeDaemon, owner.Mode)
	require.Equal(t, "/run/sennit/test.sock", owner.Socket)
}

// TestCurrentOwner_NoLockFileYet confirms CurrentOwner reports ok=false
// rather than an error for a directory that has never been locked.
func TestCurrentOwner_NoLockFileYet(t *testing.T) {
	_, ok, err := CurrentOwner(t.TempDir())
	require.NoError(t, err)
	require.False(t, ok)
}
