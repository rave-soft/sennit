package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/stretchr/testify/require"
)

// searchPermissionFake is a minimal permission.Requester for the
// outside-working-directory gate glob/grep/ripgrep now share with ls and
// read: never confined, answers every request with the configured grant,
// and records what it was asked so a test can assert whether it was asked
// at all.
type searchPermissionFake struct {
	grant    bool
	requests []permission.CreatePermissionRequest
}

func (f *searchPermissionFake) Request(_ context.Context, req permission.CreatePermissionRequest) (bool, error) {
	f.requests = append(f.requests, req)
	return f.grant, nil
}

func (f *searchPermissionFake) ConfinedDir() string { return "" }

// searchPermissionCtx carries the session id the outside-workdir gate
// requires before it will even ask.
func searchPermissionCtx(t *testing.T) context.Context {
	t.Helper()
	return context.WithValue(t.Context(), SessionIDContextKey, "test-session")
}

// TestGlobTool_OutsideWorkingDirPromptsAndRefusesOnDenial is the glob half
// of the gap ls and read already closed: an absolute path outside the
// working directory must prompt, and a denial must refuse the call rather
// than run it anyway.
func TestGlobTool_OutsideWorkingDirPromptsAndRefusesOnDenial(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workdir := filepath.Join(root, "workdir")
	require.NoError(t, os.MkdirAll(workdir, 0o755))
	outside := filepath.Join(root, "elsewhere")
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.go"), []byte("package elsewhere\n"), 0o644))

	perms := &searchPermissionFake{grant: false}
	tool := NewGlobTool(perms, workdir, config.ToolGlob{})

	resp := runToolWith(t, tool, searchPermissionCtx(t), GlobToolName, GlobParams{Pattern: "*.go", Path: outside})
	require.True(t, resp.IsError, "denial must refuse the call")
	require.Len(t, perms.requests, 1)
	require.Equal(t, GlobToolName, perms.requests[0].ToolName)
	require.Equal(t, "list", perms.requests[0].Action)
}

// TestGlobTool_RelativeEscapeIsCaught pins the join-then-resolve case the
// task called out: "../.." joins onto the working directory rather than
// replacing it, so the gate must still catch what it resolves to.
func TestGlobTool_RelativeEscapeIsCaught(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workdir := filepath.Join(root, "a", "b", "workdir")
	require.NoError(t, os.MkdirAll(workdir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "secret.go"), []byte("package root\n"), 0o644))

	perms := &searchPermissionFake{grant: false}
	tool := NewGlobTool(perms, workdir, config.ToolGlob{})

	resp := runToolWith(t, tool, searchPermissionCtx(t), GlobToolName, GlobParams{Pattern: "*.go", Path: "../../.."})
	require.True(t, resp.IsError)
	require.Len(t, perms.requests, 1)
}

// TestGlobTool_InsidePathDoesNotPrompt is the false-positive check: normal,
// in-workspace use must not pay for the gate.
func TestGlobTool_InsidePathDoesNotPrompt(t *testing.T) {
	t.Parallel()

	workdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "in.go"), []byte("package workdir\n"), 0o644))

	perms := &searchPermissionFake{grant: false}
	tool := NewGlobTool(perms, workdir, config.ToolGlob{})

	resp := runToolWith(t, tool, searchPermissionCtx(t), GlobToolName, GlobParams{Pattern: "*.go"})
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "in.go")
	require.Empty(t, perms.requests, "an in-workspace search must not prompt")
}

// TestGrepTool_OutsideWorkingDirPromptsAndRefusesOnDenial is grep's version
// of the glob test above.
func TestGrepTool_OutsideWorkingDirPromptsAndRefusesOnDenial(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workdir := filepath.Join(root, "workdir")
	require.NoError(t, os.MkdirAll(workdir, 0o755))
	outside := filepath.Join(root, "elsewhere")
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.go"), []byte("const Secret = 1\n"), 0o644))

	perms := &searchPermissionFake{grant: false}
	tool := NewGrepTool(perms, workdir, config.ToolGrep{})

	resp := runToolWith(t, tool, searchPermissionCtx(t), GrepToolName, GrepParams{Pattern: "Secret", Path: outside})
	require.True(t, resp.IsError, "denial must refuse the call")
	require.Len(t, perms.requests, 1)
	require.Equal(t, GrepToolName, perms.requests[0].ToolName)
	require.Equal(t, "search", perms.requests[0].Action)
}

// TestGrepTool_RelativeEscapeIsCaught mirrors the glob relative-escape test.
func TestGrepTool_RelativeEscapeIsCaught(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workdir := filepath.Join(root, "a", "b", "workdir")
	require.NoError(t, os.MkdirAll(workdir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "secret.go"), []byte("const Secret = 1\n"), 0o644))

	perms := &searchPermissionFake{grant: false}
	tool := NewGrepTool(perms, workdir, config.ToolGrep{})

	resp := runToolWith(t, tool, searchPermissionCtx(t), GrepToolName, GrepParams{Pattern: "Secret", Path: "../../.."})
	require.True(t, resp.IsError)
	require.Len(t, perms.requests, 1)
}

// TestGrepTool_InsidePathDoesNotPrompt is grep's false-positive check.
func TestGrepTool_InsidePathDoesNotPrompt(t *testing.T) {
	t.Parallel()

	workdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "in.go"), []byte("const InWorkdir = 1\n"), 0o644))

	perms := &searchPermissionFake{grant: false}
	tool := NewGrepTool(perms, workdir, config.ToolGrep{})

	resp := runToolWith(t, tool, searchPermissionCtx(t), GrepToolName, GrepParams{Pattern: "InWorkdir"})
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "in.go")
	require.Empty(t, perms.requests, "an in-workspace search must not prompt")
}

// ripgrepStubCommand returns a fake rg process (a deterministic stand-in
// for rg's JSON output protocol) that reports one match in dir, so the
// granted/no-prompt cases can assert a real result without depending on rg
// being installed.
//
// It re-execs the test binary itself into
// TestSearchConfinementRipgrepStubHelper below, the same convention
// TestRipgrepFixtureHelper in pagination_e2e_test.go already uses, rather
// than shelling out through `sh -c`: `sh` is not guaranteed to be on PATH
// on the Windows CI runner, and a `sh -c` stub silently produces no rg
// output there — which is exactly what made
// TestRipgrepTool_InsidePathDoesNotPrompt fail on windows-latest ("No
// files found" instead of a hit): the stub was never a working
// replacement for rg on that platform, not a gap in the confinement gate
// itself.
func ripgrepStubCommand(dir string) func(ctx context.Context, pattern, path, include string, caseInsensitive bool) *exec.Cmd {
	return func(ctx context.Context, pattern, path, include string, caseInsensitive bool) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSearchConfinementRipgrepStubHelper$", "--", filepath.Join(dir, "hit.go"))
	}
}

// TestSearchConfinementRipgrepStubHelper is not a real test: it is the
// re-exec target ripgrepStubCommand launches to stand in for rg, and does
// nothing when run as an ordinary test (no "--" argument present).
func TestSearchConfinementRipgrepStubHelper(t *testing.T) {
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		return
	}
	require.Len(t, os.Args[separator+1:], 1)
	path := os.Args[separator+1]

	record := ripgrepMatch{Type: "match"}
	record.Data.Path.Text = path
	record.Data.Lines.Text = "const Hit = 1\n"
	record.Data.LineNumber = 1
	record.Data.Submatches = append(record.Data.Submatches, struct {
		Start int `json:"start"`
	}{Start: 0})
	require.NoError(t, json.NewEncoder(os.Stdout).Encode(record))
	os.Exit(0)
}

// TestRipgrepTool_OutsideWorkingDirPromptsAndRefusesOnDenial confirms
// ripgrep — which does not share a code path with grep.NewGrepTool, it only
// falls back to it when rg is missing from $PATH — has its own gate rather
// than relying on that fallback.
func TestRipgrepTool_OutsideWorkingDirPromptsAndRefusesOnDenial(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workdir := filepath.Join(root, "workdir")
	require.NoError(t, os.MkdirAll(workdir, 0o755))
	outside := filepath.Join(root, "elsewhere")
	require.NoError(t, os.MkdirAll(outside, 0o755))

	perms := &searchPermissionFake{grant: false}
	// The denied path must return before ever invoking rg, so a command
	// stub that would fail the test if run is deliberate here.
	failCommand := func(ctx context.Context, pattern, path, include string, caseInsensitive bool) *exec.Cmd {
		t.Fatal("ripgrep must not run once permission is denied")
		return nil
	}
	tool := NewRipgrepTool(perms, workdir, config.ToolGrep{}, withRipgrepCommand(failCommand))

	resp := runToolWith(t, tool, searchPermissionCtx(t), RipgrepToolName, RipgrepParams{Pattern: "Secret", Path: outside})
	require.True(t, resp.IsError, "denial must refuse the call")
	require.Len(t, perms.requests, 1)
	require.Equal(t, RipgrepToolName, perms.requests[0].ToolName)
	require.Equal(t, "search", perms.requests[0].Action)
}

// TestRipgrepTool_RelativeEscapeIsCaught mirrors the glob/grep
// relative-escape tests.
func TestRipgrepTool_RelativeEscapeIsCaught(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workdir := filepath.Join(root, "a", "b", "workdir")
	require.NoError(t, os.MkdirAll(workdir, 0o755))

	perms := &searchPermissionFake{grant: false}
	failCommand := func(ctx context.Context, pattern, path, include string, caseInsensitive bool) *exec.Cmd {
		t.Fatal("ripgrep must not run once permission is denied")
		return nil
	}
	tool := NewRipgrepTool(perms, workdir, config.ToolGrep{}, withRipgrepCommand(failCommand))

	resp := runToolWith(t, tool, searchPermissionCtx(t), RipgrepToolName, RipgrepParams{Pattern: "Secret", Path: "../../.."})
	require.True(t, resp.IsError)
	require.Len(t, perms.requests, 1)
}

// TestRipgrepTool_InsidePathDoesNotPrompt is ripgrep's false-positive
// check, exercised end to end through the stub rg process.
func TestRipgrepTool_InsidePathDoesNotPrompt(t *testing.T) {
	t.Parallel()

	workdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "hit.go"), []byte("const Hit = 1\n"), 0o644))

	perms := &searchPermissionFake{grant: false}
	tool := NewRipgrepTool(perms, workdir, config.ToolGrep{}, withRipgrepCommand(ripgrepStubCommand(workdir)))

	resp := runToolWith(t, tool, searchPermissionCtx(t), RipgrepToolName, RipgrepParams{Pattern: "Hit"})
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "hit.go")
	require.Empty(t, perms.requests, "an in-workspace search must not prompt")
}
