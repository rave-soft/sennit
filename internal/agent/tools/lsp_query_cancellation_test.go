package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/stretchr/testify/require"
)

// lspQueryTestWorktree writes a single file that plainly does not contain
// "NoSuchSymbolAnywhere", so a lookup for it is a genuine miss (empty grep
// results) rather than a search failure.
func lspQueryTestWorktree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package e2e\n\nfunc Something() {}\n"), 0o644))
	return root
}

// canceledCtx returns a context that is already done, plus the session id
// the write-capable tools (rename) require to get past their own
// missing-session-ID check before reaching symbol resolution.
func canceledCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), SessionIDContextKey, "sess"))
	cancel()
	return ctx
}

// TestLSPQueryTools_PropagateCancellationAsGoError is the regression test
// for the "not found" text response swallowing cancellation: resolveSymbol
// wraps every failure from its underlying grep walk the same way,
// including ctx.Err() raised mid-walk (see grep.go's visitSearchMatches),
// so a canceled call used to come back indistinguishable from a symbol
// that genuinely does not exist. See errSymbolNotFound in lsp_helpers.go
// and AGENTS.md's "Tool failures: text response vs. Go error".
func TestLSPQueryTools_PropagateCancellationAsGoError(t *testing.T) {
	t.Parallel()
	root := lspQueryTestWorktree(t)
	manager := lsp.NewManager(testConfigStore(t, root))
	ctx := canceledCtx(t)

	tests := []struct {
		name string
		tool fantasy.AgentTool
		call fantasy.ToolCall
	}{
		{
			"definition",
			NewDefinitionTool(manager, root),
			fantasy.ToolCall{ID: "1", Name: DefinitionToolName, Input: mustJSONInput(t, DefinitionParams{Symbol: "Something"})},
		},
		{
			"call_hierarchy",
			NewCallHierarchyTool(manager, root),
			fantasy.ToolCall{ID: "1", Name: CallHierarchyToolName, Input: mustJSONInput(t, CallHierarchyParams{Symbol: "Something", Direction: "incoming"})},
		},
		{
			"references",
			NewReferencesTool(manager, root),
			fantasy.ToolCall{ID: "1", Name: ReferencesToolName, Input: mustJSONInput(t, ReferencesParams{Symbol: "Something"})},
		},
		{
			"rename",
			NewRenameTool(manager, &mockPermissionService{}, nil, nil, root),
			fantasy.ToolCall{ID: "1", Name: RenameToolName, Input: mustJSONInput(t, RenameParams{Symbol: "Something", NewName: "Renamed"})},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resp, err := tt.tool.Run(ctx, tt.call)
			require.Error(t, err, "cancellation must abort the batch, not return a text result")
			require.True(t, errors.Is(err, context.Canceled), "error: %v", err)
			require.Equal(t, fantasy.ToolResponse{}, resp)
		})
	}
}

// TestLSPQueryTools_GenuineMissStaysTextResponse pins the other side of
// the split introduced by errSymbolNotFound: a symbol that truly has no
// grep matches must still come back as the tool's usual "not found" text,
// not a Go error.
func TestLSPQueryTools_GenuineMissStaysTextResponse(t *testing.T) {
	t.Parallel()
	root := lspQueryTestWorktree(t)
	manager := lsp.NewManager(testConfigStore(t, root))
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "sess")

	tests := []struct {
		name    string
		tool    fantasy.AgentTool
		call    fantasy.ToolCall
		isError bool
		want    string
	}{
		{
			"definition",
			NewDefinitionTool(manager, root),
			fantasy.ToolCall{ID: "1", Name: DefinitionToolName, Input: mustJSONInput(t, DefinitionParams{Symbol: "NoSuchSymbolAnywhere"})},
			false,
			"No definition found for symbol 'NoSuchSymbolAnywhere'",
		},
		{
			"call_hierarchy",
			NewCallHierarchyTool(manager, root),
			fantasy.ToolCall{ID: "1", Name: CallHierarchyToolName, Input: mustJSONInput(t, CallHierarchyParams{Symbol: "NoSuchSymbolAnywhere", Direction: "incoming"})},
			true,
			"Symbol 'NoSuchSymbolAnywhere' not found",
		},
		{
			"references",
			NewReferencesTool(manager, root),
			fantasy.ToolCall{ID: "1", Name: ReferencesToolName, Input: mustJSONInput(t, ReferencesParams{Symbol: "NoSuchSymbolAnywhere"})},
			false,
			"Symbol 'NoSuchSymbolAnywhere' not found",
		},
		{
			"rename",
			NewRenameTool(manager, &mockPermissionService{}, nil, nil, root),
			fantasy.ToolCall{ID: "1", Name: RenameToolName, Input: mustJSONInput(t, RenameParams{Symbol: "NoSuchSymbolAnywhere", NewName: "Renamed"})},
			true,
			"Symbol 'NoSuchSymbolAnywhere' not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resp, err := tt.tool.Run(ctx, tt.call)
			require.NoError(t, err)
			require.Equal(t, tt.isError, resp.IsError, resp.Content)
			require.Contains(t, resp.Content, tt.want)
		})
	}
}
