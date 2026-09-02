package tools

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/stretchr/testify/require"
)

// cancelingResourceLister stands in for the real *mcp.Registry, failing the
// way an in-flight MCP call does when the caller's own context is
// cancelled.
type cancelingResourceLister struct{ err error }

func (c cancelingResourceLister) ListResources(context.Context, mcp.ConfigProvider, string) ([]*mcp.Resource, error) {
	return nil, c.err
}

// TestListMCPResources_PropagatesCancellationAsGoError is the regression
// test for the "context canceled" text result: ListResources failing with
// context.Canceled used to come back as an ordinary tool response instead
// of aborting the batch. See AGENTS.md's "Tool failures: text response vs.
// Go error".
func TestListMCPResources_PropagatesCancellationAsGoError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tool := NewListMCPResourcesTool(testConfigStore(t, dir), cancelingResourceLister{err: context.Canceled}, nil)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "sess")

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "1", Name: ListMCPResourcesToolName, Input: mustJSONInput(t, ListMCPResourcesParams{MCPName: "srv"})})
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled))
	require.Equal(t, fantasy.ToolResponse{}, resp)
}

// TestListMCPResources_GenuineFailureStaysTextResponse pins the other side
// of the split: an ordinary MCP failure (not cancellation) must still come
// back as a text response the model can react to.
func TestListMCPResources_GenuineFailureStaysTextResponse(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tool := NewListMCPResourcesTool(testConfigStore(t, dir), cancelingResourceLister{err: errors.New("mcp: no such server")}, nil)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "sess")

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "1", Name: ListMCPResourcesToolName, Input: mustJSONInput(t, ListMCPResourcesParams{MCPName: "srv"})})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "no such server")
}
