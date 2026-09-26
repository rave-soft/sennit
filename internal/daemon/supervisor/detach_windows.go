//go:build windows

package supervisor

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detachProcess is detachProcess's Windows counterpart: CREATE_NEW_PROCESS_GROUP
// stops Ctrl-C delivered to us from reaching the child, and DETACHED_PROCESS
// gives it no console of its own to inherit. Mirrors the pre-C1 supervisor's
// detach_windows.go (027d6155c^:internal/server/supervisor/detach_windows.go).
func detachProcess(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.CreationFlags = syscall.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS
}
