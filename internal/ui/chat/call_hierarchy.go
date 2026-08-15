package chat

import (
	"encoding/json"

	tools "github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/ui/styles"
)

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
