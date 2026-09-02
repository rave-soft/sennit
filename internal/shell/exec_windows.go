//go:build windows

package shell

import (
	"os/exec"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// KillTimeout matches mvdan's DefaultExecHandler default. Exported so
// callers outside this package can derive their own timing from it — see
// the comment on the Unix definition in exec_unix.go.
const KillTimeout = 2 * time.Second

// isolateProcess is a no-op on Windows. Session isolation via Setsid is a
// Unix-only concept; Windows uses CREATE_NEW_PROCESS_GROUP which mvdan's
// default handler already handles adequately.
func isolateProcess(_ *exec.Cmd) {}

// processGroupExecHandler returns interp.DefaultExecHandler on Windows.
func processGroupExecHandler(killTimeout time.Duration) interp.ExecHandlerFunc {
	return interp.DefaultExecHandler(killTimeout)
}
