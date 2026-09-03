package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rave-soft/sennit/internal/config"
)

type Tool = mcp.Tool

// ToolResult represents the result of running an MCP tool.
type ToolResult struct {
	Type      string
	Content   string
	Data      []byte
	MediaType string
	// IsError mirrors CallToolResult.IsError: the server ran the tool and
	// the tool itself failed. It used to be dropped on the floor, so a
	// failure came back to the model as an ordinary successful result and
	// it carried on as though the call had worked.
	IsError bool
}

// Tools returns all available MCP tools.
func (r *Registry) Tools() iter.Seq2[string, []*Tool] {
	snapshot := r.CatalogSnapshot()
	return func(yield func(string, []*Tool) bool) {
		for name, tools := range snapshot.Tools {
			if !yield(name, tools) {
				return
			}
		}
	}
}

// RunTool runs an MCP tool with the given input parameters.
func (r *Registry) RunTool(ctx context.Context, cfg ConfigProvider, name, toolName string, input string) (ToolResult, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return ToolResult{}, fmt.Errorf("error parsing parameters: %w", err)
	}

	c, err := r.getOrRenewClient(ctx, cfg, name)
	if err != nil {
		return ToolResult{}, err
	}
	result, err := c.CallTool(ctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		return ToolResult{}, err
	}

	if len(result.Content) == 0 {
		return ToolResult{Type: "text", Content: "", IsError: result.IsError}, nil
	}

	var textParts []string
	var imageData []byte
	var imageMimeType string
	var audioData []byte
	var audioMimeType string

	for _, v := range result.Content {
		switch content := v.(type) {
		case *mcp.TextContent:
			textParts = append(textParts, content.Text)
		case *mcp.ImageContent:
			if imageData == nil {
				imageData = content.Data
				imageMimeType = content.MIMEType
			}
		case *mcp.AudioContent:
			if audioData == nil {
				audioData = content.Data
				audioMimeType = content.MIMEType
			}
		case *mcp.EmbeddedResource:
			textParts = append(textParts, formatEmbeddedResource(content))
		case *mcp.ResourceLink:
			textParts = append(textParts, formatResourceLink(content))
		default:
			textParts = append(textParts, fmt.Sprintf("%v", v))
		}
	}

	textContent := strings.Join(textParts, "\n")
	// G13: the same byte cap that bounds a single embedded resource
	// (formatEmbeddedResource above) must also bound the joined result -
	// a TextContent part on its own, with no embedded resource involved,
	// can just as easily be an oversized file dump handed back verbatim.
	if truncated, wasTruncated := truncateResourceText(textContent); wasTruncated {
		textContent = truncated + fmt.Sprintf("\n\n[Content truncated to %d bytes]", MaxResourceContentBytes)
	}

	// We need to make sure the data is base64
	// when using something like docker + playwright the data was not returned correctly.
	if imageData != nil {
		return ToolResult{
			Type:      "image",
			Content:   textContent,
			Data:      ensureRawBytes(imageData),
			MediaType: imageMimeType,
			IsError:   result.IsError,
		}, nil
	}

	if audioData != nil {
		return ToolResult{
			Type:      "media",
			Content:   textContent,
			Data:      ensureRawBytes(audioData),
			MediaType: audioMimeType,
			IsError:   result.IsError,
		}, nil
	}

	return ToolResult{
		Type:    "text",
		Content: textContent,
		IsError: result.IsError,
	}, nil
}

// RefreshTools gets the updated list of tools from the MCP and updates the
// global state.
func (r *Registry) RefreshTools(ctx context.Context, cfg ConfigProvider, name string) {
	owner, session, ok := r.sessionOwner(name)
	if !ok {
		slog.Warn("Refresh tools: no session", "name", name)
		return
	}
	tools, err := getTools(ctx, session)
	if err != nil {
		r.failStateForSession(name, owner, session, err)
		return
	}
	m, ok := cfg.Config().MCP[name]
	if !ok {
		return
	}
	tools = filterTools(m, tools)
	if err := validateToolSchemas(tools); err != nil {
		r.failStateForSession(name, owner, session, err)
		return
	}
	publishSingleCatalog(r, r.allTools, name, owner, session, tools, func(c *Counts, n int) { c.Tools = n })
}

func getTools(ctx context.Context, session *ClientSession) ([]*Tool, error) {
	// Always call ListTools to get the actual available tools.
	// The InitializeResult Capabilities.Tools field may be an empty object {},
	// which is valid per MCP spec, but we still need to call ListTools to discover tools.
	result, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return nil, err
	}
	return result.Tools, nil
}

// filterTools filters tools based on enabled_tools (allow list) and
// disabled_tools (deny list) from the MCP config.
func filterTools(mcpCfg config.MCPConfig, tools []*Tool) []*Tool {
	if len(mcpCfg.EnabledTools) > 0 {
		filtered := make([]*Tool, 0, len(mcpCfg.EnabledTools))
		for _, tool := range tools {
			if slices.Contains(mcpCfg.EnabledTools, tool.Name) {
				filtered = append(filtered, tool)
			}
		}
		tools = filtered
	}

	if len(mcpCfg.DisabledTools) > 0 {
		filtered := make([]*Tool, 0, len(tools))
		for _, tool := range tools {
			if !slices.Contains(mcpCfg.DisabledTools, tool.Name) {
				filtered = append(filtered, tool)
			}
		}
		tools = filtered
	}

	return tools
}

// ensureRawBytes normalizes MCP media data into raw binary bytes.
//
// The MCP Go SDK's json.Unmarshal normally base64-decodes
// ImageContent.Data into raw bytes automatically. However, some MCP
// transports (notably Docker over stdio) can deliver data in
// unexpected formats. This function handles both cases:
//
//   - If data looks like a valid base64 string (ASCII-only, decodable)
//     it is decoded and the raw bytes are returned.
//   - If data is already raw binary (contains bytes > 127) it is
//     returned as-is.
func ensureRawBytes(data []byte) []byte {
	if len(data) == 0 {
		return data
	}

	normalized := normalizeBase64Input(data)
	if decoded, ok := decodeBase64(normalized); ok {
		return decoded
	}

	// Already raw binary — return unchanged.
	return data
}

func normalizeBase64Input(data []byte) []byte {
	normalized := strings.Join(strings.Fields(string(data)), "")
	return []byte(normalized)
}

func decodeBase64(data []byte) ([]byte, bool) {
	if len(data) == 0 {
		return data, true
	}

	for _, b := range data {
		if b > 127 {
			return nil, false
		}
	}

	s := string(data)
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err == nil {
		return decoded, true
	}
	decoded, err = base64.RawStdEncoding.DecodeString(s)
	if err == nil {
		return decoded, true
	}
	return nil, false
}
