package chat

import (
	"encoding/json"
	"strings"

	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/diff"
	"github.com/rave-soft/braid/internal/fsext"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// ReplaceSymbolToolMessageItem is a message item that represents a replace symbol tool call.
type ReplaceSymbolToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*ReplaceSymbolToolMessageItem)(nil)

// NewReplaceSymbolToolMessageItem creates a new [ReplaceSymbolToolMessageItem].
func NewReplaceSymbolToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &ReplaceSymbolToolRenderContext{}, canceled)
}

// ReplaceSymbolToolRenderContext renders replace symbol tool messages.
type ReplaceSymbolToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (r *ReplaceSymbolToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	// Replace symbol uses full width for diffs, like edit.
	if opts.IsPending() {
		return pendingTool(sty, "Replace Symbol", opts.Anim, opts.Compact)
	}

	var params tools.ReplaceSymbolParams
	_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)

	file := fsext.PrettyPath(params.FilePath)
	header := toolHeader(sty, opts.Status, "Replace Symbol", width, opts, params.Symbol, file)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
		return joinToolParts(header, earlyState)
	}

	if !opts.HasResult() {
		return header
	}

	// Summarize from diff metadata when available; always one line — no
	// diff preview in chat.
	var meta tools.ReplaceSymbolResponseMetadata
	if err := json.Unmarshal([]byte(opts.Result.Metadata), &meta); err == nil && meta.OldContent != "" || meta.NewContent != "" {
		_, additions, removals := diff.GenerateDiff(meta.OldContent, meta.NewContent, file)
		header = appendResultSummary(sty, header, diffSummary(additions, removals))
	} else {
		header = appendResultSummary(sty, header, lineCountSummary(opts.Result.Content))
	}

	// On error, the inline error tail is the one exception to "no body".
	if opts.Result.IsError {
		errLine := toolErrorContent(sty, opts.Result, width)
		return strings.Join([]string{header, "", errLine}, "\n")
	}

	return header
}
