package chat

import (
	"encoding/json"

	"github.com/rave-soft/braid/internal/message"
	tools "github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// DefinitionToolMessageItem is a message item that represents a definition tool call.
type DefinitionToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*DefinitionToolMessageItem)(nil)

// NewDefinitionToolMessageItem creates a new [DefinitionToolMessageItem].
func NewDefinitionToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &DefinitionToolRenderContext{}, canceled)
}

// DefinitionToolRenderContext renders definition tool messages.
type DefinitionToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (r *DefinitionToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, "Find Definition", opts.Anim, opts.Compact)
	}

	var params tools.DefinitionParams
	_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)

	header := toolHeader(sty, opts.Status, "Find Definition", cappedWidth, opts, params.Symbol)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
		return joinToolParts(header, earlyState)
	}

	if opts.HasEmptyResult() {
		return header
	}

	// Try to summarize from metadata (syntax-highlighted code metadata),
	// falling back to the raw result content.
	var meta tools.DefinitionResponseMetadata
	if err := json.Unmarshal([]byte(opts.Result.Metadata), &meta); err == nil && meta.Content != "" {
		return appendResultSummary(sty, header, lineCountSummary(meta.Content))
	}

	return appendResultSummary(sty, header, lineCountSummary(opts.Result.Content))
}
