package fsext

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"

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
