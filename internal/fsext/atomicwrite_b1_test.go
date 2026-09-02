package fsext

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/lock"
	"github.com/stretchr/testify/require"
)

func TestAtomicWriteFileIfUnchangedReplacesExistingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o751))
	require.NoError(t, AtomicWriteFileIfUnchanged(path, []byte("old"), []byte("new"), 0o751, true))
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "new", string(content))
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(path)
		require.NoError(t, statErr)
		require.Equal(t, os.FileMode(0o751), info.Mode().Perm())
	}
}

func TestAtomicWriteFileIfUnchangedRejectsChangeBeforeFinalCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))

	conditionalReplaceState.Lock()
	originalHook := conditionalReplaceState.beforeCheck
	conditionalReplaceState.beforeCheck = func(hookPath string) {
		if hookPath == path {
			require.NoError(t, os.WriteFile(path, []byte("external"), 0o644))
		}
	}
	conditionalReplaceState.Unlock()
	t.Cleanup(func() {
		conditionalReplaceState.Lock()
		conditionalReplaceState.beforeCheck = originalHook
		conditionalReplaceState.Unlock()
	})

	err := AtomicWriteFileIfUnchanged(path, []byte("old"), []byte("new"), 0o644, true)
	require.ErrorIs(t, err, ErrFileChanged)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "external", string(content))
}

func TestAtomicWriteFileIfUnchangedDoesNotRetryFailedRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))

	conditionalReplaceState.Lock()
	originalRename := conditionalReplaceState.rename
	attempts := 0
	conditionalReplaceState.rename = func(_, target string) error {
		attempts++
		require.NoError(t, os.WriteFile(target, []byte("external"), 0o644))
		return syscall.EACCES
	}
	conditionalReplaceState.Unlock()
	t.Cleanup(func() {
		conditionalReplaceState.Lock()
		conditionalReplaceState.rename = originalRename
		conditionalReplaceState.Unlock()
	})

	err := AtomicWriteFileIfUnchanged(path, []byte("old"), []byte("new"), 0o644, true)
	require.ErrorIs(t, err, syscall.EACCES)
	require.Equal(t, 1, attempts)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "external", string(content))
}

func TestAtomicWriteFileIfUnchangedReportsFinalReadFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))
	readFailure := syscall.EIO

	conditionalReplaceState.Lock()
	originalReadFile := conditionalReplaceState.readFile
	conditionalReplaceState.readFile = func(string) ([]byte, error) {
		return nil, readFailure
	}
	conditionalReplaceState.Unlock()
	t.Cleanup(func() {
		conditionalReplaceState.Lock()
		conditionalReplaceState.readFile = originalReadFile
		conditionalReplaceState.Unlock()
	})

	err := AtomicWriteFileIfUnchanged(path, []byte("old"), []byte("new"), 0o644, true)
	require.ErrorIs(t, err, readFailure)
	require.NotErrorIs(t, err, ErrFileChanged)
	require.Contains(t, err.Error(), "read destination before replacement")
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "old", string(content))
}

func TestAtomicWriteFileIfUnchangedRejectsStaleContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(path, []byte("external"), 0o644))
	err := AtomicWriteFileIfUnchanged(path, []byte("old"), []byte("new"), 0o644, true)
	require.ErrorIs(t, err, ErrFileChanged)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "external", string(content))
}

func TestAtomicWriteFileIfUnchangedCreateRaceHasSingleWinner(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for _, content := range []string{"one", "two"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errs <- AtomicWriteFileIfUnchanged(path, nil, []byte(content), 0o644, false)
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	var successes, conflicts int
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrFileChanged):
			conflicts++
		default:
			require.NoError(t, err)
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, conflicts)
}

// TestAtomicWriteFileIfUnchangedWaitsForCrossProcessLock pins the fix for
// the cross-process TOCTOU in conditionalReplaceExisting: the file-edit
// tool (internal/agent/tools/filemutation.go) is the only real caller and
// holds no lock of its own, so conditionalReplaceExisting must take the
// cross-process flock itself around its read-compare-rename sequence.
// This simulates another process (or another sennit instance) holding
// that same flock: the write must block until it is released, then land
// cleanly rather than racing past it.
func TestAtomicWriteFileIfUnchangedWaitsForCrossProcessLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))

	release, err := lock.TryFile(conditionalReplaceLockPath(path))
	require.NoError(t, err)
	go func() {
		time.Sleep(200 * time.Millisecond)
		release()
	}()

	start := time.Now()
	err = AtomicWriteFileIfUnchanged(path, []byte("old"), []byte("new"), 0o644, true)
	require.NoError(t, err)
	require.GreaterOrEqual(t, time.Since(start), 150*time.Millisecond, "should have waited for the held flock")

	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "new", string(content))
}

// TestConditionalReplaceLockPath_StableAndDistinct pins the lock-file
// naming scheme: the same path always maps to the same lock file (so
// concurrent callers actually contend on it), and different paths never
// collide.
func TestConditionalReplaceLockPath_StableAndDistinct(t *testing.T) {
	a := filepath.Join(t.TempDir(), "a")
	b := filepath.Join(t.TempDir(), "b")

	require.Equal(t, conditionalReplaceLockPath(a), conditionalReplaceLockPath(a))
	require.NotEqual(t, conditionalReplaceLockPath(a), conditionalReplaceLockPath(b))
	require.Equal(t, conditionalReplaceLockDir, filepath.Dir(conditionalReplaceLockPath(a)))
}

// TestConditionalReplaceLockPath_CanonicalizesSymlinks pins the fix for the
// cross-process TOCTOU window that a raw filepath.Clean left open: two
// processes editing the same file through different spellings — here, a
// symlink and its target — must hash to the same lock file, or they never
// actually contend on the flock. This fails against the old
// filepath.Clean-only implementation, which treats the symlink and its
// target as unrelated paths.
func TestConditionalReplaceLockPath_CanonicalizesSymlinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	require.NoError(t, os.WriteFile(real, []byte("content"), 0o644))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(real, link))

	require.Equal(t, conditionalReplaceLockPath(real), conditionalReplaceLockPath(link))
}

// TestConditionalReplaceLockDir_NotSharedAcrossUsers pins the fix for the
// lock directory being a single fixed path under os.TempDir(): on a
// multi-user machine, whichever user ran sennit first would own that
// directory at mode 0700, and os.MkdirAll returning nil for an
// already-existing directory (it doesn't check ownership or mode) let every
// other user's flock attempt fail with EACCES on every edit. The directory
// must instead be scoped to the calling user.
func TestConditionalReplaceLockDir_NotSharedAcrossUsers(t *testing.T) {
	t.Parallel()
	require.NotEqual(t, filepath.Join(os.TempDir(), "sennit-fsext-locks"), conditionalReplaceLockDir)
}
