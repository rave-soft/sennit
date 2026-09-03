//go:build windows

package shell

import (
	"os"
	"os/exec"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// isExecutableByUser always reports true on Windows. Windows does not have
// a Unix-style execute permission bit — "can this run" comes from the file
// extension and ACLs, neither of which this models — so dispatch keeps
// deciding what to do from file contents here, same as before this check
// existed on other platforms.
func isExecutableByUser(_ os.FileInfo) bool {
	return true
}

// KillTimeout matches mvdan's DefaultExecHandler default. Exported so
// callers outside this package can derive their own timing from it — see
// the comment on the Unix definition in exec_unix.go.
const KillTimeout = 2 * time.Second

// isolateProcess is a no-op on Windows. Session isolation via Setsid is a
// Unix-only concept; there is no equivalent grouping applied here — see the
// comment on processGroupExecHandler below for what that costs.
func isolateProcess(_ *exec.Cmd) {}

// processGroupExecHandler returns interp.DefaultExecHandler unmodified on
// Windows. That handler does not set SysProcAttr, so children are not
// placed in a job object or process group of any kind, and on cancellation
// it signals only the single tracked process (cmd.Process.Signal), not a
// tree. A child that spawns its own children — and, on Windows, even an
// ordinary child, since there is no process-group kill to reach it either —
// can survive cancellation as an orphan. DefaultExecHandler also does not
// set WaitDelay on Windows, so Wait can still hang if a child keeps the
// output pipes open.
//
// Closing this gap for real needs a Job Object created with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE and the child assigned to it at spawn
// time (CreateProcess with CREATE_SUSPENDED, AssignProcessToJobObject,
// then resume) — a meaningful amount of Windows-specific syscall code that
// cannot be exercised on this repo's Linux-only dev and CI hosts. That
// should be a deliberate, separately reviewed change with its own test
// story, not folded into a comment fix.
func processGroupExecHandler(killTimeout time.Duration) interp.ExecHandlerFunc {
	return interp.DefaultExecHandler(killTimeout)
}
