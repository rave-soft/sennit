package log

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSetupMirrorsToProvidedWriter pins Setup's ws parameter: a record
// logged after Setup must land in both the rotated file and every writer
// passed in, not just the file. Before G7, nothing in the codebase ever
// called Setup with a writer — cmd/run.go instead called
// slog.SetDefault(slog.New(log.New(os.Stderr))) *after* Setup had already
// installed the file handler, which replaced it wholesale and silently
// dropped the file logger instead of adding a mirror.
//
// Setup guards its work with a package-level sync.Once, so this must be
// the only test in this package that calls it — a second call anywhere
// else in this package would be a silent no-op and could pin the wrong
// destination.
func TestSetupMirrorsToProvidedWriter(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "sennit-test.log")

	var buf bytes.Buffer
	Setup(logFile, false, &buf)

	slog.Info("Hello from the test", "k", "v")

	// slog handlers are async-safe but not synchronous with respect to
	// this goroutine's next read; Setup's handlers write inline in
	// Handle, though, so by the time slog.Info returns both writes have
	// already happened.
	require.Contains(t, buf.String(), "Hello from the test")

	data, err := os.ReadFile(logFile)
	require.NoError(t, err)
	require.Contains(t, string(data), "Hello from the test")

	// The file handler is always JSON regardless of what the extra
	// writer gets; the extra writer here is not a terminal, so it should
	// also be JSON rather than the text handler Setup reserves for a
	// real terminal file.
	var record map[string]any
	line := strings.TrimSpace(strings.Split(buf.String(), "\n")[0])
	require.NoError(t, json.Unmarshal([]byte(line), &record))
	require.Equal(t, "Hello from the test", record["msg"])
}
