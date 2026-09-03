package shell

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type conditionalBuiltinCtxKey struct{}

// TestConditionalBuiltin_FallsThroughWhenInactive pins the Defect C fix: a
// builtin registered via RegisterConditionalBuiltin must not intercept its
// name when its active condition reports false — the real program on PATH
// has to run instead of the handler silently reporting success. This is
// the mechanism shellconfig's config builtins (mcp, model, ...) rely on to
// stay out of the way of same-named PATH binaries outside a sennitrc load.
func TestConditionalBuiltin_FallsThroughWhenInactive(t *testing.T) {
	const name = "sennit_test_conditional_builtin"

	called := false
	RegisterConditionalBuiltin(name, func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
		called = true
		return nil
	}, func(ctx context.Context) bool {
		return ctx.Value(conditionalBuiltinCtxKey{}) != nil
	})

	binDir := t.TempDir()
	writeFakeExecutable(t, binDir, name, "real program ran")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout strings.Builder
	err := Run(t.Context(), RunOptions{
		Command: name,
		Cwd:     t.TempDir(),
		Env:     os.Environ(),
		Stdout:  &stdout,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if called {
		t.Fatal("inactive builtin handler was called instead of falling through")
	}
	if got := strings.TrimSpace(stdout.String()); got != "real program ran" {
		t.Fatalf("stdout = %q, want the real program's output", got)
	}
}

// TestConditionalBuiltin_InterceptsWhenActive is the companion case: the
// same registration intercepts its name once active(ctx) is true, exactly
// as config builtins do during a sennitrc load.
func TestConditionalBuiltin_InterceptsWhenActive(t *testing.T) {
	const name = "sennit_test_conditional_builtin_active"

	called := false
	RegisterConditionalBuiltin(name, func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
		called = true
		return nil
	}, func(ctx context.Context) bool {
		return ctx.Value(conditionalBuiltinCtxKey{}) != nil
	})

	binDir := t.TempDir()
	writeFakeExecutable(t, binDir, name, "real program ran")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx := context.WithValue(t.Context(), conditionalBuiltinCtxKey{}, true)
	err := Run(ctx, RunOptions{
		Command: name,
		Cwd:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !called {
		t.Fatal("active builtin handler was not called")
	}
}

// writeFakeExecutable writes a shell script named name into dir that
// prints output and makes it executable, so realCommandName-style PATH
// lookups find a real program to fall through to.
func writeFakeExecutable(t *testing.T, dir, name, output string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("PATH-based fake executable setup targets POSIX shells")
	}
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\necho " + output + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake executable: %v", err)
	}
}
