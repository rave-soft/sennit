package chat

import (
	"encoding/json"
	"fmt"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// NewMCPToolMessageItem creates a new MCP tool message item.
func NewMCPToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &MCPToolRenderContext{}, canceled)
}

// MCPToolRenderContext renders MCP tool messages.
type MCPToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (b *MCPToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	// This renderer has no config in hand (ToolRenderOpts carries only the
	// call/result, not the app's MCP server list), so it can only use
	// [proto.SplitMCPToolName]'s naive fallback — the same "first
	// underscore" split as before, still wrong for a server name that
	// itself contains an underscore. See internal/ui/dialog/permissions.go
	// for the version that resolves this correctly against config. What
	// changed here is the failure mode: a name this can't split at all no
	// longer renders as a header-less error block with no tool call shown
	// at all — it falls back to the raw tool name, same as an unrecognized
	// tool anywhere else in the transcript.
	mcpServer, mcpTool, ok := proto.SplitMCPToolName(opts.ToolCall.Name, nil)
	var name string
	if ok {
		mcpName := sty.Tool.MCPName.Render(humanizedToolName(mcpServer))
		toolName := sty.Tool.MCPToolName.Render(humanizedToolName(mcpTool))
		name = fmt.Sprintf("%s %s %s", mcpName, sty.Tool.MCPArrow.String(), toolName)
	} else {
		name = sty.Tool.MCPToolName.Render(humanizedToolName(opts.ToolCall.Name))
	}

	if opts.IsPending() {
		return pendingTool(sty, name, opts)
	}

	var params map[string]any
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, width)
	}

	var toolParams []string
	if len(params) > 0 {
		parsed, _ := json.Marshal(params)
		toolParams = append(toolParams, string(parsed))
	}

	header := toolHeader(sty, opts.Status, name, width, opts, toolParams...)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
		return joinToolParts(header, earlyState)
	}

	if !opts.HasResult() || opts.Result.Content == "" {
		return header
	}

	return appendResultSummary(sty, header, lineCountSummary(opts.Result.Content))
}
