package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestWebFetchToolNilPermissionsSkipsCheck(t *testing.T) {
	tool := NewWebFetchTool(nil, t.TempDir(), nil)

	resp, err := runWebFetchTool(t, tool, context.Background(), WebFetchParams{URL: ""})
	require.NoError(t, err)
	// No session ID in context and no permission service: the tool still
	// validates params directly instead of erroring on a missing session.
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "url is required")
}

func TestWebFetchToolDeniedPermission(t *testing.T) {
	perms := &stubPermissionService{granted: false}
	tool := NewWebFetchTool(perms, t.TempDir(), nil)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	resp, err := runWebFetchTool(t, tool, ctx, WebFetchParams{URL: "https://example.com"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.True(t, resp.StopTurn)
	require.Contains(t, resp.Content, "User denied permission")
}

func runWebFetchTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params WebFetchParams) (fantasy.ToolResponse, error) {
	t.Helper()

	input, err := json.Marshal(params)
	require.NoError(t, err)

	call := fantasy.ToolCall{
		ID:    "test-call",
		Name:  WebFetchToolName,
		Input: string(input),
	}

	return tool.Run(ctx, call)
}
