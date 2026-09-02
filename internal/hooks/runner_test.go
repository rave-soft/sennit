package hooks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// hookIgnoreSIGINTHelperEnv selects helper mode for
// TestHookIgnoreSIGINTHelperProcess (see its doc comment).
const hookIgnoreSIGINTHelperEnv = "SENNIT_HOOK_IGNORE_SIGINT_HELPER"

// TestHookIgnoreSIGINTHelperProcess is not a real test; it is a subprocess
// helper mode of this test binary, self-exec'd by
// TestRunner_IgnoredSIGINTIsNotAbandoned via os.Args[0]. It mirrors the
// pattern used by TestWorkspaceLockHelperProcess in
// internal/app/bootstrap_test.go: when the gating env var is unset, this is
// a no-op so it does not interfere with a normal `go test` run; when set,
// it reproduces a hook process that ignores SIGINT and dies only to
// SIGKILL, with no dependency on any interpreter outside the Go toolchain.
//
// A shell-builtin empty trap on INT cannot express this: SIGINT on cancellation
// is sent straight to the externally exec'd child's process group (see
// processGroupExecHandler in internal/shell/exec_unix.go), not to the
// mvdan interpreter goroutine, so the trap never sees it. This helper is
// itself the external child, so signal.Ignore applies where it matters.
func TestHookIgnoreSIGINTHelperProcess(t *testing.T) {
	if os.Getenv(hookIgnoreSIGINTHelperEnv) != "1" {
		return
	}
	signal.Ignore(os.Interrupt)
	fmt.Println("started")
	time.Sleep(5 * time.Second)
	os.Exit(0)
}

// TestRunner_DedupKeyIncludesFullConfig verifies that Run only collapses
// hooks whose entire config is identical, not merely hooks that happen to
// share a Command string. Deduping on Command alone would drop one of two
// same-command hooks that differ in name/timeout/matcher, silently
// applying the survivor's settings to both.
func TestRunner_DedupKeyIncludesFullConfig(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	r := &Runner{
		hooks: []compiledHook{
			{cfg: Hook{Name: "first", Command: "true", Timeout: 5}},
			{cfg: Hook{Name: "second", Command: "true", Timeout: 10}},
			// Exact duplicate of "first": same Name, Command, Timeout,
			// Matcher. This one really should collapse.
			{cfg: Hook{Name: "first", Command: "true", Timeout: 5}},
		},
		runShell: func(context.Context, shell.RunOptions) error {
			calls.Add(1)
			return nil
		},
		abandonFor: abandonGrace,
	}

	agg, err := r.Run(t.Context(), "PreToolUse", "session-1", "bash", "{}")
	require.NoError(t, err)

	// Two distinct hooks (first, second) should each run once; the exact
	// duplicate of "first" should not trigger a third execution.
	require.Equal(t, int32(2), calls.Load())
	require.Len(t, agg.Hooks, 2)

	names := []string{agg.Hooks[0].Name, agg.Hooks[1].Name}
	require.ElementsMatch(t, []string{"first", "second"}, names)
}

// TestRunner_BackgroundedJobDoesNotRaceOnOutputBuffers exercises a hook
// that backgrounds a job (`cmd &`) before printing its decision. mvdan.cc/sh
// does not wait for `&` jobs before Run returns, so the backgrounded job can
// still be writing to stdout/stderr after runOne's goroutine sends on
// `done` and the outer frame reads the buffers. This must use the real
// shell.Run (not a mock) so the concurrent write actually happens; run
// under -race to catch a regression to bytes.Buffer.
func TestRunner_BackgroundedJobDoesNotRaceOnOutputBuffers(t *testing.T) {
	t.Parallel()

	r := &Runner{
		hooks: []compiledHook{
			{cfg: Hook{
				Name:    "bg",
				Command: `(sleep 0.05; echo late) & echo '{"decision":"allow"}'`,
				Timeout: 5,
			}},
		},
		runShell:   shell.Run,
		cwd:        t.TempDir(),
		abandonFor: abandonGrace,
	}

	agg, err := r.Run(t.Context(), "PreToolUse", "session-1", "bash", "{}")
	require.NoError(t, err)
	require.Len(t, agg.Hooks, 1)
	require.Equal(t, "allow", agg.Hooks[0].Decision)
}

// TestRunner_IgnoredSIGINTIsNotAbandoned exercises the real signal
// escalation path (not a mock) for a hook that ignores SIGINT, e.g. via an
// empty trap on INT, or a Python/Node process installing its own handler.
//
// On timeout, shell.Run sends SIGINT and only escalates to SIGKILL after
// shell.KillTimeout. abandonGrace must be derived from that value: an
// independent, shorter grace (the historical bug was a bare
// time.Second) gives up on the goroutine before SIGKILL has even landed,
// reporting a spurious "goroutine abandoned" error and discarding whatever
// the hook wrote — for a hook that was merely slow to die, not stuck.
//
// This must run the real shell.Run so the SIGINT-then-SIGKILL sequence
// actually happens; a stubbed runShell can't exercise the signal path.
func TestRunner_IgnoredSIGINTIsNotAbandoned(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var captured bytes.Buffer

	// A shell-builtin `trap '' INT` only affects the interpreter goroutine,
	// not the external process it execs — SIGINT on cancellation is sent
	// straight to that process's group (see processGroupExecHandler), so
	// plain `sleep` would still die at t=1s regardless of any shell trap.
	// Self-exec this test binary in helper mode instead: it installs its
	// own SIG_IGN, matching the real-world case this bug was written for
	// (a Python/Node process with its own handler), with no dependency on
	// any interpreter outside the Go toolchain. The env var is set as a
	// leading command-word assignment rather than via t.Setenv, since this
	// test runs under t.Parallel() and t.Setenv forbids that.
	r := NewRunner([]Hook{{
		Command: fmt.Sprintf(
			"%s=1 %s -test.run=TestHookIgnoreSIGINTHelperProcess",
			hookIgnoreSIGINTHelperEnv, os.Args[0],
		),
		Timeout: 1,
	}}, t.TempDir(), t.TempDir())
	realRunShell := r.runShell
	r.runShell = func(ctx context.Context, opts shell.RunOptions) error {
		// Tee stdout so the test can see what the hook wrote even though
		// HookResult itself carries no output for a timed-out hook (its
		// Decision is always DecisionNone). This proves runOne's buffer
		// was written to completion rather than abandoned mid-write.
		mu.Lock()
		opts.Stdout = io.MultiWriter(opts.Stdout, &captured)
		mu.Unlock()
		return realRunShell(ctx, opts)
	}

	start := time.Now()
	agg, err := r.Run(t.Context(), "PreToolUse", "session-1", "bash", "{}")
	elapsed := time.Since(start)

	require.NoError(t, err, "a hook merely slow to die must not be reported as abandoned")
	require.Len(t, agg.Hooks, 1)
	require.Equal(t, "none", agg.Hooks[0].Decision)

	// The process can only die once SIGKILL lands at timeout + KillTimeout
	// (~3s here). It must survive at least that long without being
	// abandoned, and must complete before the old buggy 1s grace would
	// have allowed (timeout + abandonGrace, comfortably under KillTimeout
	// + abandonMargin).
	require.GreaterOrEqual(t, elapsed, time.Second+shell.KillTimeout,
		"hook should only die once SIGKILL lands")
	require.Less(t, elapsed, time.Second+abandonGrace,
		"hook should complete within timeout+abandonGrace, not be abandoned")

	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, captured.String(), "started",
		"hook's output before SIGKILL must be preserved, not discarded as if abandoned")
}
