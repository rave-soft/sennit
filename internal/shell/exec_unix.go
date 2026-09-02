//go:build !windows

package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// KillTimeout matches mvdan's DefaultExecHandler default: how long a
// cancelled child gets after SIGINT before escalating to SIGKILL. Extracted
// so the coupling to upstream is explicit rather than a buried literal, and
// exported so callers that must wait out the full SIGINT-then-SIGKILL
// window before giving up on a goroutine (see hooks.abandonGrace) can
// derive their own timing from it instead of guessing a value that has to
// stay in sync by luck.
const KillTimeout = 2 * time.Second

// isolateProcess sets SysProcAttr so the child runs in its own session,
// fully detached from Sennit's controlling terminal. This prevents shells
// like zsh from grabbing the TTY for job control, which would cause
// "suspended (tty input)" and corrupt Bubble Tea's terminal rendering.
// It also prevents child processes from sending signals to Sennit's process
// group.
func isolateProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// processGroupExecHandler returns an ExecHandlerFunc that replaces
// interp.DefaultExecHandler with one that fully isolates child processes
// from Sennit's session and controlling terminal.
//
// Without this, shells like zsh that enable job control when sourcing
// framework files will attempt to take over the TTY, causing SIGTTIN/SIGTTOU
// signals and corrupting the parent terminal state.
//
// The implementation mirrors interp.DefaultExecHandler with two additions:
// isolateProcess(&cmd) after construction, and negative-PID signal targeting
// in the cancellation callback so the entire child process group is killed.
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
			// A grandchild that re-setsid/setpgid's itself (a daemonizing
			// server, `ssh -f`, a double-forker) survives the -pid kill
			// below and can hold the stdout/stderr pipes open forever,
			// which would otherwise hang Wait indefinitely. WaitDelay
			// bounds that: once Wait observes the process exit, it force-
			// closes the pipes after this long instead of waiting on EOF.
			WaitDelay: killTimeout,
		}
		isolateProcess(&cmd)

		err = cmd.Start()
		if err == nil {
			// reaped closes once Wait has returned, which is the moment
			// the pid stops being ours: the kernel is free to hand it to
			// something else, and a SIGKILL sent to -pid after that can
			// land on an unrelated process group. The escalation waits
			// on this as well as on its timer, and the immediate branch
			// checks it before signalling.
			reaped := make(chan struct{})
			pid := cmd.Process.Pid
			stopf := context.AfterFunc(ctx, func() {
				if killTimeout <= 0 {
					select {
					case <-reaped:
						return
					default:
					}
					_ = syscall.Kill(-pid, syscall.SIGKILL)
					return
				}
				select {
				case <-reaped:
					return
				default:
				}
				// Signal the child's process group (negative PID) so
				// grandchildren also receive it.
				_ = syscall.Kill(-pid, syscall.SIGINT)
				timer := time.NewTimer(killTimeout)
				defer timer.Stop()
				select {
				case <-reaped:
					return
				case <-timer.C:
				}
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			})
			defer stopf()

			err = cmd.Wait()
			close(reaped)
		}

		return exitStatusFromError(ctx, hc.Stderr, err)
	}
}

// exitStatusFromError translates an exec error into an interp exit status,
// matching the conventions of interp.DefaultExecHandler. Extracted so the
// platform-specific exec handler stays focused on isolation mechanics.
func exitStatusFromError(ctx context.Context, stderr io.Writer, err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return interp.ExitStatus(128 + uint8(status.Signal()))
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
