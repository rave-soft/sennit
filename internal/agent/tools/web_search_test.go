package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestWebSearchToolDeniedPermission(t *testing.T) {
	perms := &stubPermissionService{granted: false}
	tool := NewWebSearchTool(perms, t.TempDir(), nil)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	resp, err := runWebSearchTool(t, tool, ctx, WebSearchParams{Query: "braid coding agent"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.True(t, resp.StopTurn)
	require.Contains(t, resp.Content, "User denied permission")
}

func TestWebSearchToolRequiresQuery(t *testing.T) {
	// nil permissions: sub-agent mode, no permission check should occur.
	tool := NewWebSearchTool(nil, t.TempDir(), nil)

	resp, err := runWebSearchTool(t, tool, context.Background(), WebSearchParams{Query: ""})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "query is required")
}

func runWebSearchTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params WebSearchParams) (fantasy.ToolResponse, error) {
	t.Helper()

	input, err := json.Marshal(params)
	require.NoError(t, err)

	call := fantasy.ToolCall{
		ID:    "test-call",
		Name:  WebSearchToolName,
		Input: string(input),
	}

	return tool.Run(ctx, call)
}
