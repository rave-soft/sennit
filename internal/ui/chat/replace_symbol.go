package chat

import (
	"encoding/json"
	"strings"

	"github.com/rave-soft/sennit/internal/diff"
	"github.com/rave-soft/sennit/internal/fsext"
	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// ReplaceSymbolToolRenderContext renders replace symbol tool messages.
type ReplaceSymbolToolRenderContext struct{}

// replaceSymbolTitle maps the tool's Action param to the header it
// renders under — "replace" is the default when Action is empty (the
// param description calls it out as such), but add_before/add_after/delete
// are meaningfully different operations and a header that always reads
// "Replace Symbol" hides a deletion as if it were an edit.
func replaceSymbolTitle(action string) string {
	switch action {
	case "add_before":
		return "Insert Before Symbol"
	case "add_after":
		return "Insert After Symbol"
	case "delete":
		return "Delete Symbol"
	default:
		return "Replace Symbol"
	}
}

// RenderTool implements the [ToolRenderer] interface.
func (r *ReplaceSymbolToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	// Replace symbol uses full width for diffs, like edit.
	var params tools.ReplaceSymbolParams
	_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)
	title := replaceSymbolTitle(params.Action)

	if opts.IsPending() {
		return pendingTool(sty, title, opts)
	}

	file := fsext.PrettyPath(params.FilePath)
	header := toolHeader(sty, opts.Status, title, width, opts, params.Symbol, file)
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
	if err := json.Unmarshal([]byte(opts.Result.Metadata), &meta); err == nil && (meta.OldContent != "" || meta.NewContent != "") {
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
