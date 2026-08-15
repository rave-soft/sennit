package chat

import (
	"encoding/json"

	"github.com/rave-soft/braid/internal/fsext"
	tools "github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// ReferencesToolRenderContext renders references tool messages.
type ReferencesToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (r *ReferencesToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, "Find References", opts.Anim, opts.Compact)
	}

	var params tools.ReferencesParams
	_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)

	toolParams := []string{params.Symbol}
	if params.Path != "" {
		toolParams = append(toolParams, "path", fsext.PrettyPath(params.Path))
	}

	header := toolHeader(sty, opts.Status, "Find References", cappedWidth, opts, toolParams...)
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
