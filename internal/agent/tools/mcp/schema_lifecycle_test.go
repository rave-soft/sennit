package mcp

import (
	"context"
	"errors"
	"testing"

	mcpapi "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/stretchr/testify/require"
)

// liveSessionWithSchema exposes a server-provided schema verbatim, including
// malformed schemas that AddTool's inferred-schema path would otherwise hide.
func liveSessionWithSchema(t *testing.T, tool *mcpapi.Tool) (*ClientSession, context.Context) {
	t.Helper()
	serverTransport, clientTransport := mcpapi.NewInMemoryTransports()
	server := mcpapi.NewServer(&mcpapi.Implementation{Name: "schema-server"}, nil)
	server.AddTool(tool, func(context.Context, *mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
		return &mcpapi.CallToolResult{}, nil
	})
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	client := mcpapi.NewClient(&mcpapi.Implementation{Name: "schema-client"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	return &ClientSession{ClientSession: clientSession, cancel: cancel}, ctx
}

func TestInvalidSchemaInitialPublishFailsClosed(t *testing.T) {
	const name = "invalid-initial"
	r := NewRegistry()
	owner, err := r.beginAttempt(name)
	require.NoError(t, err)
	session, sessionCtx := liveSessionWithSchema(t, &mcpapi.Tool{Name: "bad-tool", InputSchema: map[string]any{"type": "object", "required": "id"}})

	err = r.publishOrClose(context.Background(), name, config.MCPConfig{Type: config.MCPStdio}, owner, session)
	require.Error(t, err)
	require.ErrorContains(t, err, "bad-tool")
	// initClient performs this transition after publishOrClose fails.
	r.updateStateFor(name, owner, StateError, err)
	info, ok := r.GetState(name)
	require.True(t, ok)
	require.Equal(t, StateError, info.State)
	require.ErrorContains(t, info.Error, "bad-tool")
	require.ErrorIs(t, sessionCtx.Err(), context.Canceled)
	_, exists := r.sessions.Get(name)
	require.False(t, exists)
	snapshot := r.CatalogSnapshot()
	require.NotContains(t, snapshot.Tools, name)
	require.NotContains(t, snapshot.Prompts, name)
	require.NotContains(t, snapshot.Resources, name)
}

func TestInvalidSchemaRefreshFailsClosedWithoutClobberingNewSession(t *testing.T) {
	const name = "invalid-refresh"
	r := NewRegistry()
	cfg := config.NewTestStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})
	owner, err := r.beginAttempt(name)
	require.NoError(t, err)
	old, oldCtx := liveSessionWithSchema(t, &mcpapi.Tool{Name: "bad-tool", InputSchema: map[string]any{"type": "object", "properties": []any{}}})
	r.publishMu.Lock()
	r.sessions.Set(name, old)
	r.sessionOwners[name] = owner
	r.allTools.Set(name, []*Tool{{Name: "old"}})
	r.allPrompts.Set(name, []*Prompt{{Name: "old-prompt"}})
	r.allResources.Set(name, []*Resource{{Name: "old-resource"}})
	r.publishMu.Unlock()

	r.RefreshTools(context.Background(), cfg, name)
	info, ok := r.GetState(name)
	require.True(t, ok)
	require.Equal(t, StateError, info.State)
	require.ErrorContains(t, info.Error, "bad-tool")
	require.ErrorIs(t, oldCtx.Err(), context.Canceled)
	_, exists := r.sessions.Get(name)
	require.False(t, exists)
	snapshot := r.CatalogSnapshot()
	require.NotContains(t, snapshot.Tools, name)
	require.NotContains(t, snapshot.Prompts, name)
	require.NotContains(t, snapshot.Resources, name)

	// A stale refresh failure cannot tear down a replacement owned by a newer attempt.
	freshOwner, err := r.beginAttempt(name)
	require.NoError(t, err)
	fresh, freshCtx := liveSession(t, "fresh")
	r.publishMu.Lock()
	r.sessions.Set(name, fresh)
	r.sessionOwners[name] = freshOwner
	r.publishMu.Unlock()
	r.updateStateForSession(name, owner, old, StateError, errors.New("stale bad-tool"), Counts{})
	published, exists := r.sessions.Get(name)
	require.True(t, exists)
	require.Same(t, fresh, published)
	require.NoError(t, freshCtx.Err())
}
