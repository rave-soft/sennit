package chat

import (
	"encoding/json"

	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/message"
	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// -----------------------------------------------------------------------------
// Glob Tool
// -----------------------------------------------------------------------------

// registerSearchToolRenderers registers the glob, grep, ripgrep and ls
// tool renderers.
func registerSearchToolRenderers() {
	registerToolRenderer(tools.GlobToolName, &GlobToolRenderContext{})
	registerToolRenderer(tools.GrepToolName, &GrepToolRenderContext{title: "Grep"})
	registerToolRenderer(tools.RipgrepToolName, &GrepToolRenderContext{title: "Ripgrep"})
	registerToolRenderer(tools.LSToolName, &LSToolRenderContext{})
}

// GlobToolRenderContext renders glob tool messages.
type GlobToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (g *GlobToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	if opts.IsPending() {
		return pendingTool(sty, "Glob", opts)
	}

	var params tools.GlobParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, width)
	}

	toolParams := []string{params.Pattern}
	if params.Path != "" {
		toolParams = append(toolParams, "path", params.Path)
	}

	header := toolHeader(sty, opts.Status, "Glob", width, opts, toolParams...)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
		return joinToolParts(header, earlyState)
	}

	if !opts.HasResult() || opts.Result.Content == "" {
		return header
	}

	var meta tools.GlobResponseMetadata
	_ = json.Unmarshal([]byte(opts.Result.Metadata), &meta)
	summary := pagedCountSummary(meta.NumberOfFiles, meta.TotalFiles, "file", "files")
	summary = incompleteSuffix(summary, meta.Incomplete)
	return appendResultSummary(sty, header, summary)
}

// -----------------------------------------------------------------------------
// Grep Tool
// -----------------------------------------------------------------------------

// GrepToolRenderContext renders grep and ripgrep tool messages.
type GrepToolRenderContext struct {
	title string
}

// RenderTool implements the [ToolRenderer] interface.
func (g *GrepToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	if opts.IsPending() {
		return pendingTool(sty, g.title, opts)
	}

	// RipgrepParams is a superset of GrepParams, so it decodes both tools'
	// inputs.
	var params tools.RipgrepParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, width)
	}

	toolParams := []string{params.Pattern}
	if params.Path != "" {
		toolParams = append(toolParams, "path", params.Path)
	}
	if params.Include != "" {
		toolParams = append(toolParams, "include", params.Include)
	}
	if params.LiteralText {
		toolParams = append(toolParams, "literal", "true")
	}
	if params.CaseInsensitive {
		toolParams = append(toolParams, "case-insensitive", "true")
	}

	header := toolHeader(sty, opts.Status, g.title, width, opts, toolParams...)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
		return joinToolParts(header, earlyState)
	}

	if opts.HasEmptyResult() {
		return header
	}

	var meta tools.GrepResponseMetadata
	_ = json.Unmarshal([]byte(opts.Result.Metadata), &meta)
	summary := pagedCountSummary(meta.NumberOfMatches, meta.TotalMatches, "match", "matches")
	summary = incompleteSuffix(summary, meta.Incomplete)
	return appendResultSummary(sty, header, summary)
}

// -----------------------------------------------------------------------------
// LS Tool
// -----------------------------------------------------------------------------

// LSToolRenderContext renders ls tool messages.
type LSToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (l *LSToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	if opts.IsPending() {
		return pendingTool(sty, "List", opts)
	}

	var params tools.LSParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, width)
	}

	path := params.Path
	if path == "" {
		path = "."
	}
	path = fsext.PrettyPath(path)

	header := toolHeader(sty, opts.Status, "List", width, opts, path)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
		return joinToolParts(header, earlyState)
	}

	if opts.HasEmptyResult() {
		return header
	}

	var meta tools.LSResponseMetadata
	_ = json.Unmarshal([]byte(opts.Result.Metadata), &meta)
	summary := pagedCountSummary(meta.NumberOfFiles, meta.TotalFiles, "entry", "entries")
	summary = incompleteSuffix(summary, meta.Incomplete)
	return appendResultSummary(sty, header, summary)
}

// -----------------------------------------------------------------------------
