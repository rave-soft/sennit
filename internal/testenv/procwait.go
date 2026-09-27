package testenv

import (
	"testing"
	"time"
)

// WaitForProcessExit polls until pid no longer refers to a running
// process, failing t once timeout elapses.
//
// A daemon test cleanup that only waits for a unix socket to stop
// answering (the cross-platform way back to a process a supervisor
// deliberately released rather than kept as a child, see
// supervisor.spawnDetached) can return before the daemon process itself
// has actually exited: the socket can be unlinked a moment before the
// process closes its remaining file handles -- its own log file, a
// sennit.db connection, the parent's daemon-startup.log inherited on
// Stdout/Stderr. On Linux/macOS a still-open handle underneath a
// directory does not stop t.TempDir()'s cleanup from removing it; on
// Windows it does, which is what surfaced this as "TempDir RemoveAll
// cleanup: ... being used by another process" in CI. Call this (with the
// PID workspacelock.CurrentOwner reports for the project) after asking a
// spawned daemon to shut down and before returning from the test/cleanup
// that spawned it.
func WaitForProcessExit(t testing.TB, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if !processRunning(pid) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("testenv: pid %d did not exit within %s", pid, timeout)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
