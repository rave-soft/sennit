//go:build windows

package testenv

import "golang.org/x/sys/windows"

// processRunning reports whether pid still names a running process.
//
// This deliberately does not use (*os.Process).Signal(syscall.Signal(0)),
// the POSIX kill(pid, 0) idiom internal/lsp's lifecycle tests use: on
// Windows, os/exec_windows.go's Process.signal only implements os.Kill
// and returns "not supported by windows" for every other signal value
// unconditionally, so that probe would report "not running" immediately
// regardless of whether pid is actually alive. Opening the process with
// only SYNCHRONIZE and checking whether its termination handle is
// signaled reflects real process state instead.
func processRunning(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// No such process: it already exited (or the PID never existed).
		return false
	}
	defer windows.CloseHandle(h)

	event, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return false
	}
	// WAIT_TIMEOUT means the wait expired without the handle becoming
	// signaled, i.e. the process is still running; WAIT_OBJECT_0 means
	// it already exited.
	return event == uint32(windows.WAIT_TIMEOUT)
}
