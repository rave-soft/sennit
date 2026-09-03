package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/config"
)

// chmodUnreadableDir creates a subdirectory under workspace that cannot be
// read, alongside a sibling file, and restores its permissions on cleanup so
// t.TempDir() can remove it. It skips on Windows (chmod does not restrict
// directory access there) and when running as root (root ignores the mode
// bit and would read the directory anyway).
func chmodUnreadableDir(t *testing.T, workspace string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not restrict directory access on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "visible.go"), []byte("x"), 0o644))
	locked := filepath.Join(workspace, "locked")
	require.NoError(t, os.Mkdir(locked, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(locked, "secret.go"), []byte("x"), 0o644))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(locked, 0o755)) })
}

// TestLSToolReportsIncompleteOnUnreadableSubdir traces the fsext-level
// incompleteness signal (fsext.VisitDirectory) all the way to the ls tool's
// response: the model must see LSResponseMetadata.Incomplete and a note in
// the output text, not just a shorter-than-expected tree with no
// explanation.
func TestLSToolReportsIncompleteOnUnreadableSubdir(t *testing.T) {
	workspace := t.TempDir()
	chmodUnreadableDir(t, workspace)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "s1")
	tool := NewLsTool(nil, workspace, config.ToolLs{})
	resp := runToolWith(t, tool, ctx, LSToolName, LSParams{})
	require.False(t, resp.IsError, resp.Content)

	metadata := responseMetadata[LSResponseMetadata](t, resp.Metadata)
	require.True(t, metadata.Incomplete, "an unreadable subdirectory must be reported as incomplete")
	require.False(t, metadata.Truncated, "incompleteness is a different fact from the result limit cutting the listing short")
	require.Contains(t, resp.Content, "could not be read")
}

// TestLSToolReportsCompleteOnFullyReadableTree is the companion case: an
// ordinary, fully-readable tree must not be reported as incomplete.
func TestLSToolReportsCompleteOnFullyReadableTree(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "sub", "file.go"), []byte("x"), 0o644))

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "s1")
	tool := NewLsTool(nil, workspace, config.ToolLs{})
	resp := runToolWith(t, tool, ctx, LSToolName, LSParams{})
	require.False(t, resp.IsError, resp.Content)

	metadata := responseMetadata[LSResponseMetadata](t, resp.Metadata)
	require.False(t, metadata.Incomplete)
	require.False(t, metadata.Truncated)
}

// TestGlobToolReportsIncompleteOnUnreadableSubdir is the glob tool's
// counterpart to TestLSToolReportsIncompleteOnUnreadableSubdir.
func TestGlobToolReportsIncompleteOnUnreadableSubdir(t *testing.T) {
	workspace := t.TempDir()
	chmodUnreadableDir(t, workspace)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "s1")
	tool := NewGlobTool(nil, workspace, config.ToolGlob{})
	resp := runToolWith(t, tool, ctx, GlobToolName, GlobParams{Pattern: "**/*.go"})
	require.False(t, resp.IsError, resp.Content)

	metadata := responseMetadata[GlobResponseMetadata](t, resp.Metadata)
	require.True(t, metadata.Incomplete, "an unreadable subdirectory must be reported as incomplete")
	require.False(t, metadata.Truncated, "incompleteness is a different fact from the result limit cutting the matches short")
	require.Contains(t, resp.Content, "could not be read")
}

// TestGlobToolReportsCompleteOnFullyReadableTree is the companion case for
// the glob tool.
func TestGlobToolReportsCompleteOnFullyReadableTree(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "visible.go"), []byte("x"), 0o644))

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "s1")
	tool := NewGlobTool(nil, workspace, config.ToolGlob{})
	resp := runToolWith(t, tool, ctx, GlobToolName, GlobParams{Pattern: "**/*.go"})
	require.False(t, resp.IsError, resp.Content)

	metadata := responseMetadata[GlobResponseMetadata](t, resp.Metadata)
	require.False(t, metadata.Incomplete)
	require.False(t, metadata.Truncated)
}

// TestGrepToolReportsIncompleteOnUnreadableSubdir is the grep tool's
// counterpart to TestLSToolReportsIncompleteOnUnreadableSubdir: grep's pure
// Go walk (used when rg is not on $PATH — see visitSearchMatches in
// grep.go) must surface the same signal instead of silently under-reporting
// matches.
func TestGrepToolReportsIncompleteOnUnreadableSubdir(t *testing.T) {
	workspace := t.TempDir()
	chmodUnreadableDir(t, workspace)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "hit.go"), []byte("needle"), 0o644))

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "s1")
	tool := NewGrepTool(nil, workspace, config.ToolGrep{})
	resp := runToolWith(t, tool, ctx, GrepToolName, GrepParams{Pattern: "needle"})
	require.False(t, resp.IsError, resp.Content)

	metadata := responseMetadata[GrepResponseMetadata](t, resp.Metadata)
	require.True(t, metadata.Incomplete, "an unreadable subdirectory must be reported as incomplete")
	require.False(t, metadata.Truncated, "incompleteness is a different fact from the result limit cutting the matches short")
	require.Contains(t, resp.Content, "could not be read")
}

// TestGrepToolReportsCompleteOnFullyReadableTree is the companion case for
// the grep tool.
func TestGrepToolReportsCompleteOnFullyReadableTree(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "hit.go"), []byte("needle"), 0o644))

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "s1")
	tool := NewGrepTool(nil, workspace, config.ToolGrep{})
	resp := runToolWith(t, tool, ctx, GrepToolName, GrepParams{Pattern: "needle"})
	require.False(t, resp.IsError, resp.Content)

	metadata := responseMetadata[GrepResponseMetadata](t, resp.Metadata)
	require.False(t, metadata.Incomplete)
	require.False(t, metadata.Truncated)
}
