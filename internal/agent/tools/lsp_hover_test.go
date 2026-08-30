package tools

import (
	"context"
	"encoding/json"
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
