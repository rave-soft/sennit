package shell

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// handlerCtx returns a context carrying a real interp.HandlerContext (Dir
// set to dir, env inherited from the process), the same way builtinHandler
// in run.go hands one to every builtin. handleJQ reads its cwd/env through
// interp.HandlerCtx, which panics on a bare context, so any test that calls
// handleJQ directly (rather than through [Run]) needs one of these — a
// trivial script triggers the exec handler exactly once, which is enough to
// capture the ctx it was given.
func handlerCtx(t *testing.T, dir string) context.Context {
	t.Helper()

	var captured context.Context
	runner, err := interp.New(
		interp.Dir(dir),
		interp.Env(expand.ListEnviron(os.Environ()...)),
		interp.ExecHandlers(func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
			return func(ctx context.Context, args []string) error {
				captured = ctx
				return nil
			}
		}),
	)
	if err != nil {
		t.Fatalf("interp.New: %v", err)
	}
	line, err := syntax.NewParser().Parse(strings.NewReader("__handler_ctx_probe__"), "")
	if err != nil {
		t.Fatalf("parse probe command: %v", err)
	}
	if err := runner.Run(t.Context(), line); err != nil {
		t.Fatalf("run probe command: %v", err)
	}
	if captured == nil {
		t.Fatal("exec handler was never invoked; no HandlerContext captured")
	}
	return captured
}

// TestJQ_CtxCancel verifies that handleJQ polls ctx during iteration and
// returns ctx.Err() (not an interp.ExitStatus) when the context is
// cancelled. This is what lets hook timeouts interrupt long-running jq
// filters rather than waiting for the iterator to terminate naturally.
func TestJQ_CtxCancel(t *testing.T) {
	t.Parallel()

	// `range(N)` generates a large stream of values. With a slurped input
	// the filter produces all N values in sequence; ctx cancellation
	// between values should short-circuit the loop.
	const filter = "range(10000000)"
	stdin := strings.NewReader("null\n")

	base := handlerCtx(t, t.TempDir())
	ctx, cancel := context.WithCancel(base)
	// Cancel almost immediately so we catch the next iteration check.
	cancel()

	err := handleJQ(ctx, []string{"jq", filter}, stdin, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected ctx cancel error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestJQ_CtxCancel_DuringFilter verifies cancellation mid-stream: ctx is
// cancelled after jq has started producing output, and the loop must
// observe the cancel on the next iteration rather than running to
// completion.
func TestJQ_CtxCancel_DuringFilter(t *testing.T) {
	t.Parallel()

	base := handlerCtx(t, t.TempDir())
	ctx, cancel := context.WithTimeout(base, 50*time.Millisecond)
	defer cancel()

	// 100M values; without ctx polling this would take many seconds to
	// fully emit. With ctx polling the loop exits shortly after the
	// deadline.
	stdin := strings.NewReader("null\n")
	var stdout, stderr bytes.Buffer

	start := time.Now()
	err := handleJQ(ctx, []string{"jq", "-c", "range(100000000)"}, stdin, &stdout, &stderr)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected ctx timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	// Allow generous slack for slow CI; the important invariant is that we
	// don't run all 100M iterations (which would take orders of magnitude
	// longer than 1s).
	if elapsed > time.Second {
		t.Fatalf("handleJQ took %v after 50ms timeout; ctx polling is not tight enough", elapsed)
	}
}

// slowReader serves bytes in small chunks with a fixed delay between
// Read calls. It never blocks indefinitely — each Read returns after
// chunkDelay — so cancellation must be observed via ctxReader's ctx
// check, not by the underlying reader itself. That isolates the
// behavior we want to test: the wrapper polling ctx between chunks.
type slowReader struct {
	remaining  []byte
	chunk      int
	chunkDelay time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	if len(s.remaining) == 0 {
		return 0, io.EOF
	}
	time.Sleep(s.chunkDelay)
	n := min(len(p), min(s.chunk, len(s.remaining)))
	copy(p, s.remaining[:n])
	s.remaining = s.remaining[n:]
	return n, nil
}

// TestJQ_CtxCancel_MidReadAll verifies that ctx cancellation observed
// *during* io.ReadAll — after several chunks have already been consumed
// — short-circuits the read via ctxReader, rather than draining the
// whole source. This is the guarantee the hook runner relies on when
// it feeds a large bytes.Reader payload.
//
// The reader serves bytes in 512-byte chunks with a 5ms gap between
// reads. ctx is cancelled after ~50ms, so several chunks have already
// been read when ctxReader first observes the cancellation. The test
// asserts that (a) we got a context.Canceled error and (b) the call
// returned well before the reader would have been fully drained.
func TestJQ_CtxCancel_MidReadAll(t *testing.T) {
	t.Parallel()

	const (
		size       = 64 * 1024 * 1024 // 64 MiB
		chunk      = 512
		chunkDelay = 5 * time.Millisecond
	)
	// At 512 bytes / 5ms, draining 64 MiB would take ~11 minutes. Any
	// return within a second proves cancel was observed mid-stream, not
	// after EOF.
	reader := &slowReader{
		remaining:  bytes.Repeat([]byte("a"), size),
		chunk:      chunk,
		chunkDelay: chunkDelay,
	}

	base := handlerCtx(t, t.TempDir())
	ctx, cancel := context.WithCancel(base)
	defer cancel()

	// Cancel after enough time that several Read calls have completed
	// and io.ReadAll is actively consuming the source.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := handleJQ(ctx, []string{"jq", "-R", "."}, reader, io.Discard, io.Discard)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// Generous slack for slow CI; the invariant is orders-of-magnitude
	// faster than draining the full source.
	if elapsed > time.Second {
		t.Fatalf("mid-ReadAll cancel took %v; ctxReader is not polling between chunks", elapsed)
	}
	// Sanity check: we should have been cancelled mid-stream, not
	// before any reads happened. If remaining == size, cancel fired so
	// early nothing was consumed — that's a fast-fail path, not the
	// mid-read guarantee we want to verify.
	consumed := size - len(reader.remaining)
	if consumed == 0 {
		t.Fatal("reader was never read from; test did not exercise mid-ReadAll cancel")
	}
	if consumed >= size {
		t.Fatal("reader was fully drained; cancel was not observed mid-read")
	}
}

// failOnReadReader fails the test if Read is ever called. It proves that a
// code path short-circuited before touching the input, without relying on
// wall-clock timing.
type failOnReadReader struct {
	t *testing.T
}

func (r failOnReadReader) Read(p []byte) (int, error) {
	r.t.Error("Read called; outer guard did not short-circuit before io.ReadAll")
	return 0, io.EOF
}

// TestJQ_CtxCancel_PreCancel verifies the fast-fail path: a ctx already
// cancelled before handleJQ is called returns context.Canceled
// immediately via the outer-loop guard, never entering io.ReadAll.
// Complements TestJQ_CtxCancel_MidReadAll.
//
// The invariant — not wall-clock timing — is what proves the guard fired:
// the input reader fails the test if it is ever read from. A previous
// version asserted a 100ms ceiling, which measured scheduler latency rather
// than the guard and flaked on contended Windows CI runners under -race.
func TestJQ_CtxCancel_PreCancel(t *testing.T) {
	t.Parallel()

	base := handlerCtx(t, t.TempDir())
	ctx, cancel := context.WithCancel(base)
	cancel()

	err := handleJQ(ctx, []string{"jq", "-R", "."},
		failOnReadReader{t: t},
		io.Discard, io.Discard)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestJQ_Success confirms the ctx-aware refactor did not regress the
// success path.
func TestJQ_Success(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	err := handleJQ(
		handlerCtx(t, t.TempDir()),
		[]string{"jq", "-c", ".a"},
		strings.NewReader(`{"a":1}`),
		&stdout, io.Discard,
	)
	if err != nil {
		t.Fatalf("handleJQ returned error: %v", err)
	}
	if got := stdout.String(); got != "1\n" {
		t.Fatalf("stdout = %q, want %q", got, "1\n")
	}
}

// TestJQ_UnknownFlagBeforeFilter verifies that an unrecognized flag given
// before the filter is rejected instead of silently being treated as the
// filter itself (which would then make the real filter an unexpected file
// argument).
func TestJQ_UnknownFlagBeforeFilter(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	err := handleJQ(
		handlerCtx(t, t.TempDir()),
		[]string{"jq", "-x", ".a"},
		strings.NewReader(`{"a":1}`),
		&stdout, &stderr,
	)
	if err == nil {
		t.Fatal("expected an error for an unknown flag, got nil")
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("stdout = %q, want empty", got)
	}
	if !strings.Contains(stderr.String(), "unknown option: -x") {
		t.Fatalf("stderr = %q, want it to mention the unknown option", stderr.String())
	}
}

// TestJQRawInputDropsTheTrailingNewline pins jq -R against real jq: a
// trailing newline terminates the last line rather than starting an empty
// one, so a three-line file is three strings. Splitting alone produced a
// fourth, empty one and every -R pipeline ended on a spurious "".
func TestJQRawInputDropsTheTrailingNewline(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := Run(t.Context(), RunOptions{
		Command: `printf 'a\nb\nc\n' | jq -R .`,
		Cwd:     t.TempDir(),
		Stdout:  &out,
	})
	if err != nil {
		t.Fatalf("jq -R must succeed: %v", err)
	}

	got := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	want := []string{`"a"`, `"b"`, `"c"`}
	if !slices.Equal(got, want) {
		t.Fatalf("jq -R over three lines must yield three strings, got %q", got)
	}
}

// TestJQ_UsesInterpreterCwdAndEnv verifies that jq resolves relative file
// arguments against the interpreter's cwd (not the Sennit process cwd) and
// that $ENV sees the interpreter's environment, not os.Environ(). Both
// previously came from the process directly (os.Open, os.Environ), so a
// shell whose WorkingDir/Env had diverged (e.g. after `cd` inside the
// script) got the wrong file or the wrong environment.
func TestJQ_UsesInterpreterCwdAndEnv(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "data.json"), []byte(`{"name":"pkg"}`), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var out bytes.Buffer
	err := Run(t.Context(), RunOptions{
		// jq's own cwd (dir) does not have data.json; only the interpreter's
		// cwd after `cd sub` does. This only succeeds if jq resolves the
		// relative path against hc.Dir, not the process cwd.
		Command: `cd sub && jq .name data.json`,
		Cwd:     dir,
		Stdout:  &out,
	})
	if err != nil {
		t.Fatalf("jq over a relative path after cd must succeed: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != `"pkg"` {
		t.Fatalf("jq .name data.json = %q, want %q", got, `"pkg"`)
	}

	out.Reset()
	err = Run(t.Context(), RunOptions{
		Command: `FOO=1 jq -n '$ENV.FOO'`,
		Cwd:     dir,
		Stdout:  &out,
	})
	if err != nil {
		t.Fatalf("jq -n over $ENV must succeed: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != `"1"` {
		t.Fatalf(`jq -n '$ENV.FOO' = %q, want %q`, got, `"1"`)
	}
}
