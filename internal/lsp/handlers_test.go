package lsp

import (
	"context"
	"encoding/json"
	"testing"

	powernap "github.com/charmbracelet/x/powernap/pkg/lsp"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/stretchr/testify/require"
)

// TestHandleApplyEdit_EvaluatesEncodingAtCallTime guards against capturing
// the offset encoding at registerHandlers time. registerHandlers runs
// before Initialize completes (some servers send requests during the
// handshake itself), so a value captured then is always the pre-negotiation
// default — a server with a non-default encoding (clangd's utf-8, say)
// would get edits computed at the wrong offsets. HandleApplyEdit must take
// a func it calls at request time, not a fixed value.
func TestHandleApplyEdit_EvaluatesEncodingAtCallTime(t *testing.T) {
	calls := 0
	handler := HandleApplyEdit(func() powernap.OffsetEncoding {
		calls++
		return powernap.UTF8
	})
	require.Equal(t, 0, calls, "encoding must not be evaluated before the handler is invoked")

	params, err := json.Marshal(protocol.ApplyWorkspaceEditParams{
		Edit: protocol.WorkspaceEdit{},
	})
	require.NoError(t, err)

	_, err = handler(context.Background(), "workspace/applyEdit", params)
	require.NoError(t, err)
	require.Equal(t, 1, calls, "encoding must be evaluated exactly once, at call time")
}

// TestHandleWorkspaceConfiguration_MatchesItemCount guards against
// returning a fixed one-element response regardless of how many sections
// the server asked for. Per the LSP spec, the response array must have one
// entry per requested ConfigurationItem, in order — a server asking for
// 2+ sections (e.g. gopls asking for both "gopls" and "build") would
// otherwise read its second setting from a response slot that doesn't
// correspond to it.
func TestHandleWorkspaceConfiguration_MatchesItemCount(t *testing.T) {
	params, err := json.Marshal(protocol.ConfigurationParams{
		Items: []protocol.ConfigurationItem{
			{Section: "gopls"},
			{Section: "build"},
			{Section: "formatting"},
		},
	})
	require.NoError(t, err)

	result, err := HandleWorkspaceConfiguration(context.Background(), "workspace/configuration", params)
	require.NoError(t, err)
	require.Len(t, result, 3)
}
