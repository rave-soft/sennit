package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/stretchr/testify/require"
)

// liveSessionWithContent is like liveSession (lifecycle_test.go) but the
// tool returns exactly the content the caller supplies, so tests can drive
// RunTool's content-type switch (tools.go) with SDK content types that
// don't come up in the package's other fixtures.
func liveSessionWithContent(t *testing.T, toolName string, content []mcp.Content) *ClientSession {
	t.Helper()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "srv"}, nil)
	mcp.AddTool(
		server,
		&mcp.Tool{Name: toolName, Description: "test tool"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: content}, nil, nil
		},
	)
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	client := mcp.NewClient(&mcp.Implementation{Name: "sennit-test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)

	return &ClientSession{ClientSession: clientSession, cancel: cancel}
}

// TestRunTool_EmbeddedResourceAndResourceLinkFormatting is the regression
// test for G12: *mcp.EmbeddedResource and *mcp.ResourceLink used to fall
// into RunTool's default %v branch, which prints a Go struct's memory
// address (something like "&{map[] <nil> 0xc000...}") instead of the
// resource's actual content whenever a server answered a tool call with
// either type - e.g. a get_file tool returning the file as an embedded
// resource.
func TestRunTool_EmbeddedResourceAndResourceLinkFormatting(t *testing.T) {
	t.Parallel()
	const name = "embedded-resource-server"
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})

	sess := liveSessionWithContent(t, "get_file", []mcp.Content{
		&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
			URI:      "file:///project/README.md",
			MIMEType: "text/plain",
			Text:     "hello from the embedded resource",
		}},
		&mcp.ResourceLink{URI: "file:///project/LICENSE", Name: "LICENSE"},
	})
	owner, err := r.beginAttempt(name)
	require.NoError(t, err)
	require.NoError(t, r.publishSession(context.Background(), name, config.MCPConfig{Type: config.MCPStdio}, owner, sess))

	result, err := r.RunTool(context.Background(), cfg, name, "get_file", `{}`)
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Contains(t, result.Content, "hello from the embedded resource")
	require.Contains(t, result.Content, "file:///project/LICENSE")
	require.Contains(t, result.Content, "LICENSE")
	require.NotContains(t, result.Content, "0x", "must not print a raw pointer dump for either content type")
	require.NotContains(t, result.Content, "map[]", "must not print the zero-value Meta field of a %v dump")
}

// TestRunTool_TextContentIsTruncated is the regression test for the second
// half of G13: the byte cap on an embedded resource's text must also bound
// RunTool's overall joined text result, since a plain TextContent part (no
// embedded resource involved) can just as easily be an oversized dump
// handed back verbatim.
func TestRunTool_TextContentIsTruncated(t *testing.T) {
	t.Parallel()
	const name = "oversized-text-server"
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})

	oversized := strings.Repeat("a", MaxResourceContentBytes+1024)
	sess := liveSessionWithContent(t, "dump", []mcp.Content{&mcp.TextContent{Text: oversized}})
	owner, err := r.beginAttempt(name)
	require.NoError(t, err)
	require.NoError(t, r.publishSession(context.Background(), name, config.MCPConfig{Type: config.MCPStdio}, owner, sess))

	result, err := r.RunTool(context.Background(), cfg, name, "dump", `{}`)
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.LessOrEqual(t, len(result.Content), MaxResourceContentBytes+64)
	require.Contains(t, result.Content, "truncated")
}

func TestEnsureRawBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    []byte
		wantData []byte
	}{
		{
			name:     "already base64 encoded",
			input:    []byte("SGVsbG8gV29ybGQh"), // "Hello World!" in base64
			wantData: []byte("Hello World!"),
		},
		{
			name:     "raw binary data (PNG header)",
			input:    []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A},
			wantData: []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A},
		},
		{
			name:     "raw binary with high bytes",
			input:    []byte{0xFF, 0xD8, 0xFF, 0xE0}, // JPEG header
			wantData: []byte{0xFF, 0xD8, 0xFF, 0xE0},
		},
		{
			name:     "empty data",
			input:    []byte{},
			wantData: []byte{},
		},
		{
			name:     "base64 with padding",
			input:    []byte("YQ=="), // "a" in base64
			wantData: []byte("a"),
		},
		{
			name:     "base64 without padding",
			input:    []byte("YQ"),
			wantData: []byte("a"),
		},
		{
			name:     "base64 with whitespace",
			input:    []byte("U0dWc2JHOGdWMjl5YkdRaA==\n"),
			wantData: []byte("SGVsbG8gV29ybGQh"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := ensureRawBytes(tt.input)
			require.Equal(t, tt.wantData, result)

			if len(result) > 0 && !bytes.Equal(result, tt.input) {
				reEncoded := base64.StdEncoding.EncodeToString(result)
				_, err := base64.StdEncoding.DecodeString(reEncoded)
				require.NoError(t, err, "re-encoded result should be valid base64")
			}
		})
	}
}

func TestFilterTools(t *testing.T) {
	t.Parallel()

	tools := []*Tool{
		{Name: "tool_a"},
		{Name: "tool_b"},
		{Name: "tool_c"},
	}

	t.Run("no filters returns all tools", func(t *testing.T) {
		t.Parallel()
		result := filterTools(config.MCPConfig{}, tools)
		require.Len(t, result, 3)
	})

	t.Run("disabled tools filters deny list", func(t *testing.T) {
		t.Parallel()
		result := filterTools(config.MCPConfig{DisabledTools: []string{"tool_a"}}, tools)
		require.Len(t, result, 2)
		require.Equal(t, "tool_b", result[0].Name)
		require.Equal(t, "tool_c", result[1].Name)
	})

	t.Run("enabled tools acts as allow list", func(t *testing.T) {
		t.Parallel()
		result := filterTools(config.MCPConfig{EnabledTools: []string{"tool_b"}}, tools)
		require.Len(t, result, 1)
		require.Equal(t, "tool_b", result[0].Name)
	})

	t.Run("enabled and disabled both apply", func(t *testing.T) {
		t.Parallel()
		result := filterTools(config.MCPConfig{
			EnabledTools:  []string{"tool_a", "tool_b"},
			DisabledTools: []string{"tool_b"},
		}, tools)
		require.Len(t, result, 1)
		require.Equal(t, "tool_a", result[0].Name)
	})

	t.Run("enabled with non-existent tool returns empty", func(t *testing.T) {
		t.Parallel()
		result := filterTools(config.MCPConfig{EnabledTools: []string{"non_existent"}}, tools)
		require.Len(t, result, 0)
	})
}
