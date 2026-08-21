package log

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPanicLogPathUsesConfiguredLogDir(t *testing.T) {
	// Not run in parallel: mutates the package-level logDir shared with
	// other tests in this file.
	dir := t.TempDir()
	logDir.Store(dir)
	t.Cleanup(func() { logDir.Store("") })

	got := panicLogPath("sennit-panic-test.log")
	require.Equal(t, filepath.Join(dir, "sennit-panic-test.log"), got)
}

func TestRecoverPanicWritesToConfiguredLogDir(t *testing.T) {
	// Not run in parallel: mutates the package-level logDir shared with
	// other tests in this file.
	dir := t.TempDir()
	logDir.Store(dir)
	t.Cleanup(func() { logDir.Store("") })

	func() {
		defer RecoverPanic("test", nil)
		panic("boom")
	}()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Contains(t, entries[0].Name(), "sennit-panic-test-")

	// Ensure the file did not end up in the process's cwd instead.
	cwd, err := os.Getwd()
	require.NoError(t, err)
	cwdEntries, err := os.ReadDir(cwd)
	require.NoError(t, err)
	for _, e := range cwdEntries {
		require.NotContains(t, e.Name(), "sennit-panic-test-")
	}
}

func TestRecoverPanicRunsCleanupOnSuccess(t *testing.T) {
	// Not run in parallel: mutates the package-level logDir shared with
	// other tests in this file.
	dir := t.TempDir()
	logDir.Store(dir)
	t.Cleanup(func() { logDir.Store("") })

	cleaned := false
	func() {
		defer RecoverPanic("test-cleanup-success", func() { cleaned = true })
		panic("boom")
	}()

	require.True(t, cleaned, "cleanup should run when the panic log file is written successfully")
}

func TestRecoverPanicRunsCleanupWhenLogFileCannotBeCreated(t *testing.T) {
	// Not run in parallel: mutates the package-level logDir shared with
	// other tests in this file.
	//
	// Point logDir at a path that is itself a regular file, so
	// panicLogPath returns <file>/<name>.log and os.Create fails with
	// ENOTDIR.
	notADir := filepath.Join(t.TempDir(), "not-a-dir-file")
	require.NoError(t, os.WriteFile(notADir, []byte("x"), 0o644))
	logDir.Store(notADir)
	t.Cleanup(func() { logDir.Store("") })

	cleaned := false
	func() {
		defer RecoverPanic("test-cleanup-failure", func() { cleaned = true })
		panic("boom")
	}()

	require.True(t, cleaned, "cleanup should run even when the panic log file cannot be created")
}
