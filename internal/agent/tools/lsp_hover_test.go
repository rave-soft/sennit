package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/stretchr/testify/require"
)

func TestHoverContentsMarkup(t *testing.T) {
	if got := hoverContents(&protocol.Hover{Contents: protocol.MarkupContent{Kind: protocol.Markdown, Value: "`f()`"}}); got != "`f()`" {
		t.Fatalf("got %q", got)
	}
}

// TestHoverTool_RejectsWorkspaceEscape is the regression test for the
// workspace check catching only an exact ".." but not a deeper escape like
// "../x" or an absolute path outside root: filepath.Rel(root, path) for
// either of those resolves to a "../..."-shaped rel, not a bare "..", so
// the old `rel == ".."` comparison let them through. Aligns with the
// correct form already used in lsp_workspace_symbols.go.
func TestHoverTool_RejectsWorkspaceEscape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	tool := NewHoverTool(nil, root)

	for _, tc := range []struct {
		name     string
		filePath string
	}{
		{"exact parent escape", ".."},
		{"deeper escape", "../x"},
		{"deeper escape into a sibling file", "../../etc/passwd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			input, err := json.Marshal(HoverParams{FilePath: tc.filePath, Line: 0, Character: 0})
			require.NoError(t, err)
			resp, err := tool.Run(context.Background(), fantasy.ToolCall{Input: string(input)})
			require.NoError(t, err)
			require.True(t, resp.IsError, "an escaping file_path must be refused")
			require.Contains(t, resp.Content, "file_path must be inside the workspace")
		})
	}
}

// TestHoverTool_RejectsNonPositiveLineOrCharacter is the regression test
// for the fix to requests.Hover: once that method started subtracting one
// to reach the LSP wire's 0-based position, an unvalidated line: 0 (which
// is also HoverParams.Line's omitempty zero value, so a model that just
// omits line reaches the same path) would underflow to line 4294967295
// instead of being caught here.
func TestHoverTool_RejectsNonPositiveLineOrCharacter(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	file := filepath.Join(root, "a.go")
	require.NoError(t, os.WriteFile(file, []byte("package a\n"), 0o644))
	tool := NewHoverTool(nil, root)

	for _, tc := range []struct {
		name       string
		line, char int
	}{
		{"line omitted", 0, 1},
		{"character omitted", 1, 0},
		{"both omitted", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			input, err := json.Marshal(HoverParams{FilePath: "a.go", Line: tc.line, Character: tc.char})
			require.NoError(t, err)
			resp, err := tool.Run(context.Background(), fantasy.ToolCall{Input: string(input)})
			require.NoError(t, err)
			require.True(t, resp.IsError, "a non-positive line or character must be refused")
			require.Contains(t, resp.Content, "1-based")
		})
	}
}

// TestLSPHoverThroughManagerConvertsPosition pins the fix to
// requests.Hover: a 1-based file_path/line/character request must reach
// the server as a 0-based position, the same conversion every other
// position-based LSP request already applies.
func TestLSPHoverThroughManagerConvertsPosition(t *testing.T) {
	root := newLSPToolWorktree(t)
	logPath := filepath.Join(root, "hover.log")
	manager := newLSPToolE2EManagerWithEnv(t, root, "symbols", map[string]string{"SENNIT_LSP_TOOL_LOG": logPath})

	resp := runToolWith(t, NewHoverTool(manager, root), t.Context(), HoverToolName, HoverParams{FilePath: "a.go", Line: 3, Character: 5})
	require.False(t, resp.IsError)

	contents, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Contains(t, strings.TrimSpace(string(contents)), "line=2 character=4",
		"Hover must send the 0-based position corresponding to the 1-based file_path/line/character it was given")
}

// TestHoverTool_ContextCanceledIsGoErrorNotTextResponse is the regression
// test for D5: c.Hover's error used to be wrapped in
// fantasy.NewTextErrorResponse unconditionally, so canceling the context
// (e.g. the user hitting Esc) came back to the model as an ordinary tool
// result reading "hover failed: context canceled" instead of aborting the
// tool-call batch the way every other infrastructure failure does.
func TestHoverTool_ContextCanceledIsGoErrorNotTextResponse(t *testing.T) {
	root := newLSPToolWorktree(t)
	manager := newLSPToolE2EManager(t, root, "symbols")
	tool := NewHoverTool(manager, root)

	// Warm the client so the file is already open — a canceled context is
	// then only exercised on the Hover round trip itself, not on getting
	// the server started.
	warm := runToolWith(t, tool, t.Context(), HoverToolName, HoverParams{FilePath: "a.go", Line: 3, Character: 5})
	require.False(t, warm.IsError, warm.Content)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	input, err := json.Marshal(HoverParams{FilePath: "a.go", Line: 3, Character: 5})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{Input: string(input)})
	require.Error(t, err, "a canceled context must abort as a Go error, not a text response")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, fantasy.ToolResponse{}, resp)
}
