package lsp

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/testenv"
	"github.com/stretchr/testify/require"
)

// TestClient_CloseTimeoutFallsBackToKillWithoutPanic exercises
// closeProcessLocked's timeout branch: a server that never answers
// "shutdown" must still be torn down via Kill(), which closes the
// underlying transport.Connection.
//
// This is a regression test for a jsonrpc2 teardown race: closing that
// connection used to call jsonrpc2.Conn.Close() directly from Kill()'s
// goroutine, which could run concurrently with jsonrpc2's own read loop
// delivering a response and panic on a double close of the same
// channel. The fix makes Close() close the process stream first, so the
// read loop observes the failure and closes the jsonrpc2 connection
// itself - see third_party/powernap/pkg/transport/connection.go and
// third_party/PATCHES.md. A regression here reintroduces that panic,
// which crashes this test binary rather than failing a single test.
//
// Deliberately not t.Parallel(): LSP tests are sized for serial execution.
func TestClient_CloseTimeoutFallsBackToKillWithoutPanic(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "lsp.log")

	cfg := config.LSPConfig{
		Command: exe,
		Env: map[string]string{
			fakeLSPServerEnv:           "1",
			"SENNIT_LSP_FAKE_SCENARIO": "hang-on-shutdown",
			"SENNIT_LSP_FAKE_LOG":      logPath,
		},
	}
	resolver := config.NewShellVariableResolver(testenv.New(map[string]string{}))
	client, err := New("test-close-timeout", cfg, resolver, dir, false)
	require.NoError(t, err)

	initCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = client.Initialize(initCtx, dir)
	require.NoError(t, err)
	require.NoError(t, client.WaitForServerReady(initCtx))

	pid := fakeServerPID(t, logPath)

	// A deadline well under closeTimeout (5s) forces closeProcessLocked
	// into its timeout branch instead of waiting out the real budget.
	closeCtx, closeCancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer closeCancel()
	err = client.Close(closeCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	// The fake server never answers "shutdown", so the only way this
	// process is gone is via Kill()'s teardown. os.Process.Signal isn't
	// implemented for signal 0 on Windows, so this half of the assertion
	// only runs where it can actually observe the process table.
	if goruntime.GOOS != "windows" {
		require.Eventually(t, func() bool {
			return !processAlive(pid)
		}, 5*time.Second, 10*time.Millisecond, "fake LSP server process %d outlived Kill()", pid)
	}
}

// fakeServerPID waits for the fake server to log its first line and
// returns the PID it reported. Every logged line is prefixed with
// os.Getpid() of that single child process, so any line works.
func fakeServerPID(t *testing.T, logPath string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		contents, err := os.ReadFile(logPath)
		if err == nil {
			if line, _, ok := strings.Cut(string(contents), "\n"); ok {
				if field, _, ok := strings.Cut(line, " "); ok {
					pid, err := strconv.Atoi(field)
					require.NoError(t, err)
					return pid
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake LSP server never logged a PID (log read error: %v)", err)
		}
		time.Sleep(time.Millisecond)
	}
}

// processAlive reports whether pid still refers to a running process, by
// probing it with the null signal - this sends nothing, it only checks
// that the kernel still has a process table entry for pid.
func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}
