//go:build windows

package shell

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// longRunningChild starts a process that stays alive for well past the
// length of a test, so a test can observe something else killing it.
// ping against the loopback is the one such program present on every
// Windows image without installing anything.
func longRunningChild(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "ping", "-n", "60", "127.0.0.1")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("could not start the helper process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

// TestAssignToKillOnCloseJob_ClosingTheHandleKillsTheProcess is the first
// half of the Windows process-tree guarantee: a process placed in the job
// dies when the last handle to that job closes. Everything else here
// rests on it — the handler's deferred CloseHandle is what keeps a
// lingering grandchild from outliving a command that exited normally.
//
// This is the first test in the tree to execute this code at all: the job
// object path cannot run on the Linux hosts the rest of the suite uses.
func TestAssignToKillOnCloseJob_ClosingTheHandleKillsTheProcess(t *testing.T) {
	cmd := longRunningChild(t)

	job := assignToKillOnCloseJob(cmd.Process.Pid)
	if job == 0 {
		t.Skip("this host will not let a process be assigned to a job object; the handler documents running without the tree guarantee as the fallback")
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// Nothing has killed it yet.
	select {
	case err := <-exited:
		t.Fatalf("the helper exited before the job was closed: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := windows.CloseHandle(job); err != nil {
		t.Fatalf("closing the job handle failed: %v", err)
	}

	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the helper survived the job's last handle closing, so KILL_ON_JOB_CLOSE is not in effect")
	}
}

// TestProcessGroupExecHandler_CancelEndsTheCommand is the second half:
// a cancelled command must return rather than sit in Wait. Run goes
// through processGroupExecHandler, so this covers the handler's own
// cancellation path end to end. Before the job object, cancellation signalled the tracked
// process alone and anything it had spawned outlived the command.
func TestProcessGroupExecHandler_CancelEndsTheCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{
			Command: "ping -n 60 127.0.0.1",
			Cwd:     t.TempDir(),
			Env:     os.Environ(),
		})
	}()

	// Give the command time to actually be running before cancelling, so
	// this exercises the cancellation path rather than a context that was
	// already dead when the handler started.
	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a cancelled command never returned: the child outlived its context")
	}
}
