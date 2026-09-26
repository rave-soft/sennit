//go:build !windows

package supervisor

import (
	"os/exec"
	"syscall"
)

// detachProcess sets up c so, once started, it survives this process
// exiting: a new session (Setsid) takes it out of our process group, so
// it receives none of our signals and outlives our own controlling
// terminal. Mirrors the pre-C1 supervisor's detach_other.go
// (027d6155c^:internal/server/supervisor/detach_other.go).
func detachProcess(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setsid = true
}
