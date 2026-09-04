package tools

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/stretchr/testify/require"
)

// fakeResourceReader stands in for the real *mcp.Registry, returning
// whatever contents the test supplies without a real, connected MCP
// session.
type fakeResourceReader struct {
	contents []*mcp.ResourceContents
	err      error
}

func (f fakeResourceReader) ReadResource(context.Context, mcp.ConfigProvider, string, string) ([]*mcp.ResourceContents, error) {
	return f.contents, f.err
}

func runReadMCPResource(t *testing.T, reg mcpResourceReader) fantasy.ToolResponse {
	t.Helper()
	dir := t.TempDir()
	tool := NewReadMCPResourceTool(testConfigStore(t, dir), reg, nil)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "sess")
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID: "1", Name: ReadMCPResourceToolName,
		Input: mustJSONInput(t, ReadMCPResourceParams{MCPName: "srv", URI: "res://doc"}),
	})
	require.NoError(t, err)
	return resp
}

// TestReadMCPResource_LargeTextResourceIsTruncated is the regression test
// for G13's size cap: a text resource larger than
// mcp.MaxResourceContentBytes used to be handed to the model whole
// (string(content.Blob), no limit anywhere on this path). It must now be
// cut to the byte cap with a truncation notice, the same shape fetch.go
// uses for an oversized page.
func TestReadMCPResource_LargeTextResourceIsTruncated(t *testing.T) {
	t.Parallel()

	oversized := strings.Repeat("a", mcp.MaxResourceContentBytes+1024)
	resp := runReadMCPResource(t, fakeResourceReader{contents: []*mcp.ResourceContents{
		{URI: "res://doc", MIMEType: "text/plain", Blob: []byte(oversized)},
	}})

	require.False(t, resp.IsError)
	require.LessOrEqual(t, len(resp.Content), mcp.MaxResourceContentBytes+64)
	require.Contains(t, resp.Content, "truncated")
	require.Less(t, len(resp.Content), len(oversized), "the oversized resource must actually be cut down")
}

// TestReadMCPResource_BinaryResourceIsModelRecoverableError is the
// regression test for the other half of G13: a binary/unknown-MIME
// resource used to be decoded straight into the model's context as
// string(content.Blob), producing invalid UTF-8 (a 20 MB PDF was the
// motivating case). It must come back as a text error naming the size and
// MIME type instead of garbled bytes.
func TestReadMCPResource_BinaryResourceIsModelRecoverableError(t *testing.T) {
	t.Parallel()

	pdfBytes := []byte("%PDF-1.4\x00\x01\x02not valid utf8 \xff\xfe")
	resp := runReadMCPResource(t, fakeResourceReader{contents: []*mcp.ResourceContents{
		{URI: "res://doc.pdf", MIMEType: "application/pdf", Blob: pdfBytes},
	}})

	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "application/pdf")
	require.Contains(t, resp.Content, "res://doc.pdf")
	require.NotContains(t, resp.Content, "%PDF-1.4\x00", "the binary bytes must not be embedded verbatim")
}

// TestReadMCPResource_JSONResourceIsText pins the MIME allowlist beyond
// text/*: a JSON resource delivered as a blob must still decode as text,
// not be rejected as binary.
func TestReadMCPResource_JSONResourceIsText(t *testing.T) {
	t.Parallel()

	resp := runReadMCPResource(t, fakeResourceReader{contents: []*mcp.ResourceContents{
		{URI: "res://doc.json", MIMEType: "application/json", Blob: []byte(`{"ok":true}`)},
	}})

	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, `{"ok":true}`)
}

// TestReadMCPResource_MixedResourceReadsTextAndDescribesBinaryPart is the
// regression test for the comment/behavior mismatch found reading G13's
// fix: a resource with several parts (text plus a binary thumbnail) used
// to be rejected wholesale the moment the loop hit the first binary part,
// even though the comment above it promised only a size/MIME description
// for a binary part that isn't the sole image. The text part must survive,
// and the binary part must be described inline, not abort the read.
func TestReadMCPResource_MixedResourceReadsTextAndDescribesBinaryPart(t *testing.T) {
	t.Parallel()

	resp := runReadMCPResource(t, fakeResourceReader{contents: []*mcp.ResourceContents{
		{URI: "res://doc.md", MIMEType: "text/markdown", Text: "# hello"},
		{URI: "res://doc-thumb.png", MIMEType: "image/png", Blob: []byte{0x89, 0x50, 0x4E, 0x47}},
	}})

	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "# hello")
	require.Contains(t, resp.Content, "res://doc-thumb.png")
	require.Contains(t, resp.Content, "image/png")
}

// TestReadMCPResource_MultiPartRespectsSingleByteBudget is the regression
// test for the other half of that same fix: FormatResourceContentsText
// bounds each part to mcp.MaxResourceContentBytes on its own, but the loop
// joined every part's already-bounded text without ever re-checking the
// combined length - N parts near the cap could hand the model N times the
// intended budget. The joined response must respect one shared cap.
func TestReadMCPResource_MultiPartRespectsSingleByteBudget(t *testing.T) {
	t.Parallel()

	partSize := mcp.MaxResourceContentBytes/2 + 1024
	resp := runReadMCPResource(t, fakeResourceReader{contents: []*mcp.ResourceContents{
		{URI: "res://part1", MIMEType: "text/plain", Blob: []byte(strings.Repeat("a", partSize))},
		{URI: "res://part2", MIMEType: "text/plain", Blob: []byte(strings.Repeat("b", partSize))},
	}})

	require.False(t, resp.IsError)
	require.LessOrEqual(t, len(resp.Content), mcp.MaxResourceContentBytes+128,
		"two parts each under the per-part cap must not sum past the response's own budget")
	require.Contains(t, resp.Content, "truncated")
}

// TestReadMCPResource_ImageResourceReturnsImageResponse pins the image
// path G13 asked for: a resource whose MIME type is an image must come
// back as a normal tool image response (like the read tool's own image
// handling), not as a text description or an error.
func TestReadMCPResource_ImageResourceReturnsImageResponse(t *testing.T) {
	t.Parallel()

	imgBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	dir := t.TempDir()
	tool := NewReadMCPResourceTool(testConfigStore(t, dir), fakeResourceReader{contents: []*mcp.ResourceContents{
		{URI: "res://logo.png", MIMEType: "image/png", Blob: imgBytes},
	}}, nil)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "sess")
	ctx = context.WithValue(ctx, SupportsImagesContextKey, true)

	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID: "1", Name: ReadMCPResourceToolName,
		Input: mustJSONInput(t, ReadMCPResourceParams{MCPName: "srv", URI: "res://logo.png"}),
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, "image", resp.Type)
	require.Equal(t, imgBytes, resp.Data)
	require.Equal(t, "image/png", resp.MediaType)
}
