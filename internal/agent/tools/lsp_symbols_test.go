package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// TestSymbolsTool_ContextCanceledIsGoErrorNotTextResponse is the
// regression test for D5: client.DocumentSymbols' error used to be
// wrapped in fantasy.NewTextErrorResponse unconditionally, so canceling
// the context (e.g. the user hitting Esc) came back to the model as an
// ordinary tool result reading "failed to get document symbols: context
// canceled" instead of aborting the tool-call batch the way every other
// infrastructure failure does.
func TestSymbolsTool_ContextCanceledIsGoErrorNotTextResponse(t *testing.T) {
	root := newLSPToolWorktree(t)
	manager := newLSPToolE2EManager(t, root, "replace-symbol")
	tool := NewSymbolsTool(manager, root)

	// Warm the client so the file is already open — a canceled context is
	// then only exercised on the DocumentSymbols round trip itself, not on
	// getting the server started.
	warm := runToolWith(t, tool, t.Context(), SymbolsToolName, SymbolsParams{FilePath: "a.go"})
	require.False(t, warm.IsError, warm.Content)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	input, err := json.Marshal(SymbolsParams{FilePath: "a.go"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{Input: string(input)})
	require.Error(t, err, "a canceled context must abort as a Go error, not a text response")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, fantasy.ToolResponse{}, resp)
}
