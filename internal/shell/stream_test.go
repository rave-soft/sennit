package shell

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRunAndCaptureStream_StripsHerdrEnv mirrors
// TestRunAndCapture_StripsHerdrEnv for the streaming entrypoint the bash-
// mode UI actually calls.
func TestRunAndCaptureStream_StripsHerdrEnv(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/herdr.sock")
	t.Setenv("HERDR_PANE_ID", "wA:p1")

	result, err := RunAndCaptureStream(t.Context(), RunOptions{
		Command: "echo \"[$HERDR_SOCKET_PATH][$HERDR_PANE_ID]\"",
		Cwd:     t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatalf("RunAndCaptureStream returned error: %v", err)
	}
	if got := strings.TrimSpace(result.Output); got != "[][]" {
		t.Fatalf("herdr vars leaked into streamed command env: output = %q", got)
	}
}

// TestRunAndCaptureStream_Canceled pins the same cancellation contract as
// RunAndCapture for the streaming path bang mode uses.
func TestRunAndCaptureStream_Canceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	t.Cleanup(cancel)

	result, err := RunAndCaptureStream(ctx, RunOptions{
		// See TestRunAndCapture_Canceled: a pure-interpreter loop keeps
		// this portable.
		Command: "while :; do :; done",
		Cwd:     t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatalf("RunAndCaptureStream returned error: %v (want nil)", err)
	}
	if !result.Canceled {
		t.Fatal("expected result.Canceled = true")
	}
	if result.ExitCode != 130 {
		t.Fatalf("ExitCode = %d, want 130", result.ExitCode)
	}
}

// TestRunAndCaptureStream_ParseErrorSurfaces pins the same never-ran
// contract as RunAndCapture: a parse failure surfaces its message in
// StartErr rather than vanishing into an empty output block with exit 1.
func TestRunAndCaptureStream_ParseErrorSurfaces(t *testing.T) {
	result, err := RunAndCaptureStream(t.Context(), RunOptions{
		Command: `echo "unterminated`,
		Cwd:     t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatalf("RunAndCaptureStream returned error: %v (want nil)", err)
	}
	if result.StartErr == nil {
		t.Fatal("expected result.StartErr to be set for a parse failure")
	}
	if !strings.Contains(result.StartErr.Error(), "parse") {
		t.Fatalf("StartErr should mention parse: %v", result.StartErr)
	}
}
