//go:build unix

// syscall.Kill has no Windows counterpart, and the behaviour under test
// — a SIGTERM from `kill <pid>` cancelling the run context — is a POSIX
// signal contract in the first place.

package cmd

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRunSignalContext_CancelsOnSIGTERM is the regression test for
// runSignalContext: it used to listen for os.Kill (SIGKILL) instead of
// syscall.SIGTERM. SIGKILL can never be caught by signal.Notify, so that
// was a silent no-op — a plain `kill <pid>` (SIGTERM) got no graceful
// cancellation at all. Sends SIGTERM to this test process itself and
// checks the derived context is canceled.
func TestRunSignalContext_CancelsOnSIGTERM(t *testing.T) {
	ctx, cancel := runSignalContext(context.Background())
	defer cancel()

	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM))

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context was not canceled by SIGTERM")
	}
}
