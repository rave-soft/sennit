package chat

import (
	"encoding/json"

	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// LSPRestartToolMessageItem is a message item that represents a lsprestart tool call.
type LSPRestartToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*LSPRestartToolMessageItem)(nil)

// NewLSPRestartToolMessageItem creates a new [LSPRestartToolMessageItem].
func NewLSPRestartToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &LSPRestartToolRenderContext{}, canceled)
}

// LSPRestartToolRenderContext renders lsprestart tool messages.
type LSPRestartToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (r *LSPRestartToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, "Restart LSP", opts.Anim, opts.Compact)
	}

	var params tools.LSPRestartParams
	_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)

	var toolParams []string
	if params.Name != "" {
		toolParams = append(toolParams, params.Name)
	}

	header := toolHeader(sty, opts.Status, "Restart LSP", cappedWidth, opts, toolParams...)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
		return joinToolParts(header, earlyState)
	}

	if opts.HasEmptyResult() {
		return header
	}

	return appendResultSummary(sty, header, lineCountSummary(opts.Result.Content))
}
