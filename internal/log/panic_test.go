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

	got := panicLogPath("braid-panic-test.log")
	require.Equal(t, filepath.Join(dir, "braid-panic-test.log"), got)
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
