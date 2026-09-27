//go:build !windows

package testenv

import (
	"errors"
	"syscall"
)

// processRunning reports whether pid still names a running process.
//
// The daemon tests this backs spawn a real child, then have
// supervisor.spawnDetached's production code call (*os.Process).Release
// on it (by design: a real daemon must outlive whoever spawned it) --
// which tells Go's exec package to stop tracking it, but does nothing at
// the OS level. This test process is still that child's real parent, so
// once it exits it becomes a zombie -- and kill(pid, 0) reports a zombie
// as existing just like a live process, forever, since nothing ever
// calls wait() on it. A first, non-blocking Wait4 reaps it if it has
// already exited (observing that reap is itself the "no longer running"
// answer); ECHILD means either it was already reaped by an earlier call
// here or pid was never our child to begin with, so kill(pid, 0) is the
// fallback existence check for that case.
func processRunning(pid int) bool {
	var status syscall.WaitStatus
	wpid, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	switch {
	case errors.Is(err, syscall.ECHILD):
		return syscall.Kill(pid, 0) == nil
	case err != nil:
		return false
	case wpid == pid:
		return false
	default:
		// wpid == 0: still our child, still running.
		return true
	}
}
