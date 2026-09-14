//go:build windows

package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
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

// isolateProcess puts the child in a process group of its own, the
// closest Windows analogue to the Unix Setsid this mirrors. Two things
// follow from it: a Ctrl+C at Sennit's own console no longer reaches the
// child (the child is no longer in Sennit's group), and the child's
// group can be signalled on its own with CTRL_BREAK — which is what
// gives cancellation something to try before it terminates the job
// outright. See processGroupExecHandler.
func isolateProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

// processGroupExecHandler mirrors the Unix handler of the same name:
// interp.DefaultExecHandler's body, plus isolation at spawn and a
// cancellation path that reaches the whole tree rather than one process.
//
// Windows has no process group to kill by negative PID, so the tree is
// held by a Job Object created with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
// Every process the child spawns is in the job too (a child inherits its
// parent's job), so terminating the job — or merely closing the last
// handle to it — takes the grandchildren with it. Before this, cancelling
// signalled only the single tracked process, and anything it had spawned
// outlived the command as an orphan.
//
// What this does not close: the child is assigned to the job immediately
// after CreateProcess returns, not before it runs. Windows offers no way
// to resume a CREATE_SUSPENDED process through os/exec — Go does not hand
// back the main thread handle — so a grandchild spawned in the first
// instants of the child's life can escape the job. The window is
// microseconds and the alternative is enumerating the process's threads
// by hand to resume them; this is the trade, recorded rather than hidden.
//
// Assignment failing is not fatal to the command: a sandboxed or
// already-jobbed process (an older Windows without nested jobs, some CI
// containers) still runs, just without the tree guarantee, exactly as it
// did before this existed. Refusing to run the user's command over
// missing cleanup would be the worse trade.
func processGroupExecHandler(killTimeout time.Duration) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		hc := interp.HandlerCtx(ctx)
		path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
		if err != nil {
			fmt.Fprintln(hc.Stderr, err)
			return interp.ExitStatus(127)
		}

		cmd := exec.Cmd{
			Path:   path,
			Args:   args,
			Env:    execEnvList(hc.Env),
			Dir:    hc.Dir,
			Stdin:  hc.Stdin,
			Stdout: hc.Stdout,
			Stderr: hc.Stderr,
			// A process that escaped the job (see the doc comment) can
			// hold the stdout/stderr pipes open after the child exits,
			// which would otherwise hang Wait forever. WaitDelay bounds
			// that: once Wait observes the process exit, it force-closes
			// the pipes after this long instead of waiting on EOF.
			// DefaultExecHandler sets no WaitDelay at all on Windows.
			WaitDelay: killTimeout,
		}
		isolateProcess(&cmd)

		if err := cmd.Start(); err != nil {
			return exitStatusFromError(ctx, hc.Stderr, err)
		}

		pid := cmd.Process.Pid
		job := assignToKillOnCloseJob(pid)
		if job != 0 {
			// Closing the last handle to a KILL_ON_JOB_CLOSE job kills
			// whatever is still in it. This is the backstop for the
			// ordinary path too: a command that exits normally while a
			// grandchild lingers does not leave that grandchild behind.
			defer windows.CloseHandle(job) //nolint:errcheck // nothing to do if the handle is already gone
		}

		// reaped closes once Wait has returned. After that the pid is no
		// longer ours and a console event addressed to it could land on
		// an unrelated process group, so every signalling path checks it
		// first — the same guard the Unix handler documents.
		reaped := make(chan struct{})
		stopf := context.AfterFunc(ctx, func() {
			select {
			case <-reaped:
				return
			default:
			}
			if killTimeout > 0 {
				// CTRL_BREAK is the graceful half, standing in for the
				// Unix SIGINT: it reaches the child's own process group
				// (which isolateProcess gave it) and lets a well-behaved
				// program shut itself down. Console programs that ignore
				// it, and anything with no console at all, simply do not
				// react — hence the timer below.
				_ = windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
				timer := time.NewTimer(killTimeout)
				defer timer.Stop()
				select {
				case <-reaped:
					return
				case <-timer.C:
				}
			}
			if job != 0 {
				// The whole tree at once, which is the point of the job.
				_ = windows.TerminateJobObject(job, 1)
				return
			}
			// No job: fall back to what DefaultExecHandler did, which
			// reaches the tracked process and nothing else.
			_ = cmd.Process.Kill()
		})
		defer stopf()

		err = cmd.Wait()
		close(reaped)

		return exitStatusFromError(ctx, hc.Stderr, err)
	}
}

// assignToKillOnCloseJob puts pid in a new Job Object configured to kill
// everything in it when its last handle closes, and returns the job
// handle. It returns 0 when any step fails; every caller treats that as
// "no tree guarantee for this command" rather than as an error, so a
// process that cannot be jobbed still runs.
func assignToKillOnCloseJob(pid int) windows.Handle {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job) //nolint:errcheck // the job is being abandoned either way
		return 0
	}
	// PROCESS_SET_QUOTA and PROCESS_TERMINATE are exactly what
	// AssignProcessToJobObject requires; asking for no more than that
	// keeps this working where the process is otherwise protected.
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		windows.CloseHandle(job) //nolint:errcheck // as above
		return 0
	}
	defer windows.CloseHandle(proc) //nolint:errcheck // the job holds its own reference to the process
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		windows.CloseHandle(job) //nolint:errcheck // as above
		return 0
	}
	return job
}

// exitStatusFromError translates an exec error into an interp exit
// status, matching the conventions of interp.DefaultExecHandler. The
// Unix file has its own version: there a killed process reports the
// signal that killed it and the status encodes 128+signal, which has no
// Windows equivalent — a terminated process here just has an exit code.
// What both must agree on is that a process killed because the context
// ended reports the context's error rather than an exit status, so a
// cancelled command does not read as "the command returned non-zero".
func exitStatusFromError(ctx context.Context, stderr io.Writer, err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return interp.ExitStatus(uint8(exitErr.ExitCode()))
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		fmt.Fprintf(stderr, "%v\n", execErr)
		return interp.ExitStatus(127)
	}
	return err
}
