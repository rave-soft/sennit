//go:build unix

// syscall.Kill has no Windows counterpart, and the behaviour under test —
// an unhandled SIGTERM killing the process under its default disposition
// once stop() has unregistered getLoginContext's handler — is a POSIX
// signal contract in the first place.

package cmd

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestGetLoginContext_StopUnregistersTheSignalHandler pins the fix for
// getLoginContext discarding signal.NotifyContext's stop func: without
// calling it, the process keeps trapping SIGTERM/SIGINT forever, so a
// second `sennit login` in the same process (e.g. under test, or any
// future long-lived host) would pile up one more handler per call.
//
// This runs in a subprocess because it sends itself a real SIGTERM: with
// the fix, stop() has already unregistered getLoginContext's handler, so
// the default disposition applies and the process dies from the signal.
// Without the fix, the leaked handler intercepts it, the process survives,
// and reaches the exit(42) below instead.
func TestGetLoginContext_StopUnregistersTheSignalHandler(t *testing.T) {
	if os.Getenv("SENNIT_TEST_SIGNAL_CHILD") == "1" {
		_, stop := getLoginContext()
		stop()
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		// The kernel delivers the signal asynchronously; give it a moment
		// to actually terminate us under the default disposition before
		// concluding it was caught instead.
		time.Sleep(500 * time.Millisecond)
		os.Exit(42)
	}

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestGetLoginContext_StopUnregistersTheSignalHandler")
	cmd.Env = append(os.Environ(), "SENNIT_TEST_SIGNAL_CHILD=1")
	err := cmd.Run()

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "child must not exit cleanly")
	require.NotEqual(t, 42, exitErr.ExitCode(), "child reached os.Exit(42), meaning the SIGTERM was caught instead of killing it — the signal handler leaked")
}
