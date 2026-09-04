package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestLatestGlobalLogFileSkipsOwnProcessLog pins the promise this
// function's doc comment makes: a reader asking "which log is the sennit
// that is running" must never be answered with its own. Every command
// that loads config now installs a file logger, so the reader's own file
// exists and is the newest one in the directory.
func TestLatestGlobalLogFileSkipsOwnProcessLog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", filepath.Join(dir, "sennit.json"))

	other := filepath.Join(GlobalLogDir(), "sennit-1.log")
	require.NoError(t, os.MkdirAll(GlobalLogDir(), 0o755))
	require.NoError(t, os.WriteFile(other, []byte("someone else\n"), 0o644))
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(other, old, old))

	own := GlobalLogFile()
	require.NoError(t, os.WriteFile(own, []byte("mine\n"), 0o644))

	require.Equal(t, other, LatestGlobalLogFile(),
		"the newest log is this process's own, and must be skipped")
}
