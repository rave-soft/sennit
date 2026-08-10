package chat

import (
	"encoding/json"

	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// CallHierarchyToolMessageItem is a message item that represents a call hierarchy tool call.
type CallHierarchyToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*CallHierarchyToolMessageItem)(nil)

// NewCallHierarchyToolMessageItem creates a new [CallHierarchyToolMessageItem].
func NewCallHierarchyToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &CallHierarchyToolRenderContext{}, canceled)
}

// CallHierarchyToolRenderContext renders call hierarchy tool messages.
type CallHierarchyToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (r *CallHierarchyToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, "Call Hierarchy", opts.Anim, opts.Compact)
	}

	var params tools.CallHierarchyParams
	_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)

	direction := "incoming"
	if params.Direction == "outgoing" {
		direction = "outgoing"
	}
	header := toolHeader(sty, opts.Status, "Call Hierarchy", cappedWidth, opts, params.Symbol, direction)
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
