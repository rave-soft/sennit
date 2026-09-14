package chat

import (
	"encoding/json"
	"fmt"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// NewMCPToolMessageItem creates a new MCP tool message item. cfg supplies
// the configured MCP server names, without which the composite tool name
// can only be split at the first underscore; it may be nil.
func NewMCPToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
	cfg CustomAgentConfig,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &MCPToolRenderContext{cfg: cfg}, canceled)
}

// MCPToolRenderContext renders MCP tool messages.
type MCPToolRenderContext struct {
	// cfg names the configured MCP servers, read once per render rather
	// than captured at construction: a server added, renamed or removed
	// while the transcript is on screen has to change how the rows
	// already in it split their names.
	cfg CustomAgentConfig
}

// knownServers returns the configured MCP server names, or nil when this
// renderer was built without config (a test, or an item constructed before
// the workspace was wired up). nil is a valid input to
// proto.SplitMCPToolName — it just falls back to the naive split.
func (b *MCPToolRenderContext) knownServers() []string {
	if b.cfg == nil {
		return nil
	}
	return b.cfg.MCPServerNames()
}

// RenderTool implements the [ToolRenderer] interface.
func (b *MCPToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	// Split against the configured server names, so a server whose own
	// name contains an underscore ("my_server_tool") lands on the real
	// boundary — the transcript used to pass nil here and split at the
	// first underscore, disagreeing with the permission dialog about the
	// same call. A name that matches no configured server still splits
	// naively rather than erroring: an old session's call can name a
	// server since renamed or removed, and a name this cannot split at
	// all falls back to the raw tool name, the same as an unrecognized
	// tool anywhere else in the transcript.
	mcpServer, mcpTool, ok := proto.SplitMCPToolName(opts.ToolCall.Name, b.knownServers())
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
