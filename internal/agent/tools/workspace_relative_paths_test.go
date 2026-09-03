package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/permission"
)

// TestSearchToolsResolveRelativePathsAgainstTheWorkspace pins the search
// tools against the tree they are supposed to look in. A thread runs its
// agent in its own worktree while the process cwd stays in the main
// checkout, so a relative path taken raw searched the wrong tree
// entirely — the answer came back from the checkout the agent was not
// working in, or empty.
func TestSearchToolsResolveRelativePathsAgainstTheWorkspace(t *testing.T) {
	t.Parallel()

	// The "worktree" the tools must search, and a decoy under the
	// process's own cwd is unnecessary: pointing "sub" at a workspace
	// that has it, from a process cwd that does not, is enough.
	workspace := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "sub"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspace, "sub", "hit.go"),
		[]byte("package sub\n\nconst Needle = 1\n"), 0o644))

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "s1")

	t.Run("grep", func(t *testing.T) {
		t.Parallel()
		tool := NewGrepTool(nil, workspace, config.ToolGrep{})
		resp := runToolWith(t, tool, ctx, GrepToolName, GrepParams{Pattern: "Needle", Path: "sub"})
		require.False(t, resp.IsError, resp.Content)
		require.Contains(t, resp.Content, "hit.go")
	})

	t.Run("glob", func(t *testing.T) {
		t.Parallel()
		tool := NewGlobTool(nil, workspace, config.ToolGlob{})
		resp := runToolWith(t, tool, ctx, GlobToolName, GlobParams{Pattern: "*.go", Path: "sub"})
		require.False(t, resp.IsError, resp.Content)
		require.Contains(t, resp.Content, "hit.go")
	})
}

// runToolWith marshals params and runs tool, failing the test on a
// transport-level error.
func runToolWith(t *testing.T, tool fantasy.AgentTool, ctx context.Context, name string, params any) fantasy.ToolResponse {
	t.Helper()
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "call-1",
		Name:  name,
		Input: mustJSONInput(t, params),
	})
	require.NoError(t, err)
	return resp
}

// TestSymbolToolsRefuseToWriteWithoutASession pins the two LSP write
// tools against the gap every other write tool closes with
// missingSessionID: they used to check `sessionID != "" && permissions
// != nil` and, with no session in context, went ahead and wrote with no
// permission request at all.
func TestSymbolToolsRefuseToWriteWithoutASession(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(file, []byte("package main\n\nfunc Old() {}\n"), 0o644))

	perms := permission.NewPermissionService(dir, false, nil)

	// No session id in the context — the case that used to skip the prompt.
	_, err := NewReplaceSymbolTool(nil, perms, nil, nil, dir).Run(context.Background(), fantasy.ToolCall{
		ID:    "call-1",
		Name:  ReplaceSymbolToolName,
		Input: mustJSONInput(t, ReplaceSymbolParams{Symbol: "Old", FilePath: "main.go", Replacement: "func New() {}"}),
	})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "session"), "want a missing-session error, got %v", err)

	onDisk, readErr := os.ReadFile(file)
	require.NoError(t, readErr)
	require.Contains(t, string(onDisk), "func Old()", "the file must be untouched")
}
