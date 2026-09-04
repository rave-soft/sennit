package chat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/message"
	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// -----------------------------------------------------------------------------
// Read Tool
// -----------------------------------------------------------------------------

// registerReadToolRenderer registers the read tool renderer, including
// the legacy pre-rename tool name so old sessions keep rendering.
func registerReadToolRenderer() {
	registerToolRenderer(tools.ReadToolName, &ReadToolRenderContext{})
	// Sessions recorded before the rename still hold calls under the old
	// name; render them the same way. See [tools.LegacyReadToolName].
	registerToolRenderer(tools.LegacyReadToolName, &ReadToolRenderContext{})
}

// registerEditToolRenderers registers the write, edit, multi-edit and
// download tool renderers.
func registerEditToolRenderers() {
	registerToolItemFactory(tools.WriteToolName,
		func(sty *styles.Styles, tc message.ToolCall, res *message.ToolResult, canceled bool) ToolMessageItem {
			return NewWriteToolMessageItem(sty, tc, res, canceled)
		},
		&WriteToolRenderContext{})
	registerToolItemFactory(tools.EditToolName,
		func(sty *styles.Styles, tc message.ToolCall, res *message.ToolResult, canceled bool) ToolMessageItem {
			return NewEditToolMessageItem(sty, tc, res, canceled)
		},
		&EditToolRenderContext{})
	registerToolItemFactory(tools.MultiEditToolName,
		func(sty *styles.Styles, tc message.ToolCall, res *message.ToolResult, canceled bool) ToolMessageItem {
			return NewMultiEditToolMessageItem(sty, tc, res, canceled)
		},
		&MultiEditToolRenderContext{})
	registerToolRenderer(tools.DownloadToolName, &DownloadToolRenderContext{})
}

// ReadToolRenderContext renders view tool messages.
type ReadToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (v *ReadToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	if opts.IsPending() {
		return pendingTool(sty, "Read", opts)
	}

	var params tools.ReadParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, width)
	}

	file := fsext.PrettyPath(params.FilePath)
	toolParams := []string{file}
	if params.Limit != 0 {
		toolParams = append(toolParams, "limit", fmt.Sprintf("%d", params.Limit))
	}
	if params.Offset != 0 {
		toolParams = append(toolParams, "offset", fmt.Sprintf("%d", params.Offset))
	}

	header := toolHeader(sty, opts.Status, "Read", width, opts, toolParams...)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
		return joinToolParts(header, earlyState)
	}

	if !opts.HasResult() {
		return header
	}

	if opts.Result.Data != "" && strings.HasPrefix(opts.Result.MIMEType, "image/") {
		body := toolOutputImageContent(sty, opts.Result.Data, opts.Result.MIMEType)
		return joinToolParts(header, body)
	}

	// Try to get content from metadata first (contains actual file content).
	var meta tools.ReadResponseMetadata
	content := opts.Result.Content
	if err := json.Unmarshal([]byte(opts.Result.Metadata), &meta); err == nil && meta.Content != "" {
		content = meta.Content
	}

	if meta.ResourceType == tools.ReadResourceSkill {
		body := toolOutputSkillContent(sty, meta.ResourceName, meta.ResourceDescription)
		return joinToolParts(header, body)
	}

	if content == "" {
		return header
	}

	// Always one line: file content is not something this chat lets you
	// page through — open it in an editor to actually read it. Prefer the
	// metadata's own line count when available: it reflects the whole
	// file's TotalLines, while a plain count of content only counts the
	// page that was actually returned and reads as the whole file when
	// Truncated says it isn't.
	summary := lineCountSummary(content)
	if meta.TotalLines > 0 {
		summary = pagedCountSummary(strings.Count(content, "\n")+1, meta.TotalLines, "line", "lines")
	}
	if meta.Truncated {
		summary += fmt.Sprintf(" (more at offset %d)", meta.NextOffset)
	}
	return appendResultSummary(sty, header, summary)
}

// -----------------------------------------------------------------------------
// Write Tool
// -----------------------------------------------------------------------------

// WriteToolMessageItem is a message item that represents a write tool call.
type WriteToolMessageItem struct {
	*baseToolMessageItem
}

var (
	_ ToolMessageItem = (*WriteToolMessageItem)(nil)
	_ Expandable      = (*WriteToolMessageItem)(nil)
)

// NewWriteToolMessageItem creates a new [WriteToolMessageItem].
func NewWriteToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return &WriteToolMessageItem{newBaseToolMessageItem(sty, toolCall, result, &WriteToolRenderContext{}, canceled)}
}

// ToggleExpanded implements Expandable: a click on a finished write call
// toggles between the collapsed preview of the written content and the
// full content.
func (w *WriteToolMessageItem) ToggleExpanded() bool {
	return w.toggleExpanded()
}

// WriteToolRenderContext renders write tool messages.
type WriteToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (w *WriteToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	if opts.IsPending() {
		return pendingTool(sty, "Write", opts)
	}

	var params tools.WriteParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, width)
	}

	file := fsext.PrettyPath(params.FilePath)
	header := toolHeader(sty, opts.Status, "Write", width, opts, file)
	if opts.Compact {
		return header
	}

	if !opts.HasResult() {
		if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
			return joinToolParts(header, earlyState)
		}
		return header
	}

	// On error (e.g. denied permission), the inline error tail is the one
	// exception to "no body" — it's a short, non-click-driven summary of
	// what went wrong, not a content preview.
	if opts.Result.IsError {
		errLine := toolErrorContent(sty, opts.Result, width)
		return strings.Join([]string{header, "", errLine}, "\n")
	}

	// The written content sits directly under the header, capped while
	// collapsed with click-to-expand — same contract as bash output (see
	// expandableBodyContent).
	if params.Content != "" {
		header = appendResultSummary(sty, header, lineCountSummary(params.Content))
		return header + "\n" + expandableBodyContent(sty, params.Content, width, opts.Expanded, opts.Hovered)
	}

	return header
}

// -----------------------------------------------------------------------------
// Edit Tool
// -----------------------------------------------------------------------------

// EditToolMessageItem is a message item that represents an edit tool call.
type EditToolMessageItem struct {
	*baseToolMessageItem
}

var (
	_ ToolMessageItem = (*EditToolMessageItem)(nil)
	_ Expandable      = (*EditToolMessageItem)(nil)
)

// NewEditToolMessageItem creates a new [EditToolMessageItem].
func NewEditToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return &EditToolMessageItem{newBaseToolMessageItem(sty, toolCall, result, &EditToolRenderContext{}, canceled)}
}

// ToggleExpanded implements Expandable: a click on a finished edit call
// toggles between the collapsed diff preview and the full diff.
func (e *EditToolMessageItem) ToggleExpanded() bool {
	return e.toggleExpanded()
}

// EditToolRenderContext renders edit tool messages.
type EditToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (e *EditToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	// Edit tool uses full width for diffs.
	if opts.IsPending() {
		return pendingTool(sty, "Edit", opts)
	}

	var params tools.EditParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, width)
	}

	file := fsext.PrettyPath(params.FilePath)
	header := toolHeader(sty, opts.Status, "Edit", width, opts, file)
	if opts.Compact {
		return header
	}

	if !opts.HasResult() {
		if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
			return joinToolParts(header, earlyState)
		}
		return header
	}

	// Get diff stats from metadata; fall back to a line count if metadata
	// is missing/malformed.
	var meta tools.EditResponseMetadata
	if err := json.Unmarshal([]byte(opts.Result.Metadata), &meta); err != nil {
		header = appendResultSummary(sty, header, lineCountSummary(opts.Result.Content))
	} else {
		header = appendResultSummary(sty, header, diffSummary(meta.Additions, meta.Removals))
	}

	// On error (e.g. denied permission), the inline error tail is the one
	// exception to "no body" — see the Write tool above.
	if opts.Result.IsError {
		errLine := toolErrorContent(sty, opts.Result, width)
		return strings.Join([]string{header, "", errLine}, "\n")
	}

	if meta.OldContent != "" || meta.NewContent != "" {
		return header + "\n" + expandableDiffContent(sty, file, meta.OldContent, meta.NewContent, width, opts.Expanded, opts.Hovered)
	}

	return header
}

// -----------------------------------------------------------------------------
// MultiEdit Tool
// -----------------------------------------------------------------------------

// MultiEditToolMessageItem is a message item that represents a multi-edit tool call.
type MultiEditToolMessageItem struct {
	*baseToolMessageItem
}

var (
	_ ToolMessageItem = (*MultiEditToolMessageItem)(nil)
	_ Expandable      = (*MultiEditToolMessageItem)(nil)
)

// NewMultiEditToolMessageItem creates a new [MultiEditToolMessageItem].
func NewMultiEditToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return &MultiEditToolMessageItem{newBaseToolMessageItem(sty, toolCall, result, &MultiEditToolRenderContext{}, canceled)}
}

// ToggleExpanded implements Expandable — see EditToolMessageItem.
func (m *MultiEditToolMessageItem) ToggleExpanded() bool {
	return m.toggleExpanded()
}

// MultiEditToolRenderContext renders multi-edit tool messages.
type MultiEditToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (m *MultiEditToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	// MultiEdit tool uses full width for diffs.
	if opts.IsPending() {
		return pendingTool(sty, "Multi-Edit", opts)
	}

	var params tools.MultiEditParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, width)
	}

	file := fsext.PrettyPath(params.FilePath)
	toolParams := []string{file}
	if len(params.Edits) > 0 {
		toolParams = append(toolParams, "edits", fmt.Sprintf("%d", len(params.Edits)))
	}

	header := toolHeader(sty, opts.Status, "Multi-Edit", width, opts, toolParams...)
	if opts.Compact {
		return header
	}

	if !opts.HasResult() {
		if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
			return joinToolParts(header, earlyState)
		}
		return header
	}

	// Get diff stats from metadata; fall back to a line count if metadata
	// is missing/malformed.
	var meta tools.MultiEditResponseMetadata
	if err := json.Unmarshal([]byte(opts.Result.Metadata), &meta); err != nil {
		header = appendResultSummary(sty, header, lineCountSummary(opts.Result.Content))
	} else {
		summary := diffSummary(meta.Additions, meta.Removals)
		if len(meta.EditsFailed) > 0 {
			// diffSummary is "" when every edit failed (0 additions, 0
			// removals) — only prepend the "· " separator when there is a
			// diff summary to separate from. Unconditionally formatting
			// "%s · ..." left a leading "· " that collided with
			// appendResultSummary's own bullet, rendering "··" (see the
			// regression test).
			applied := fmt.Sprintf("%d/%d edits applied", meta.EditsApplied, len(params.Edits))
			if summary != "" {
				summary += " · " + applied
			} else {
				summary = applied
			}
		}
		header = appendResultSummary(sty, header, summary)
	}

	// On error (e.g. denied permission), the inline error tail is the one
	// exception to "no body" — see the Write tool above.
	if opts.Result.IsError {
		errLine := toolErrorContent(sty, opts.Result, width)
		return strings.Join([]string{header, "", errLine}, "\n")
	}

	if meta.OldContent != "" || meta.NewContent != "" {
		return header + "\n" + expandableDiffContent(sty, file, meta.OldContent, meta.NewContent, width, opts.Expanded, opts.Hovered)
	}

	return header
}

// -----------------------------------------------------------------------------
// Download Tool
// -----------------------------------------------------------------------------

// DownloadToolRenderContext renders download tool messages.
type DownloadToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (d *DownloadToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	if opts.IsPending() {
		return pendingTool(sty, "Download", opts)
	}

	var params tools.DownloadParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, width)
	}

	toolParams := []string{params.URL}
	if params.FilePath != "" {
		toolParams = append(toolParams, "file_path", fsext.PrettyPath(params.FilePath))
	}
	if params.Timeout != 0 {
		toolParams = append(toolParams, "timeout", formatTimeout(params.Timeout))
	}

	header := toolHeader(sty, opts.Status, "Download", width, opts, toolParams...)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
		return joinToolParts(header, earlyState)
	}

	if opts.HasEmptyResult() {
		return header
	}

	return appendResultSummary(sty, header, lineCountSummary(opts.Result.Content))
}

// expandableDiffContent renders an edit's diff under its header with the
// same click-to-expand contract as expandableBodyContent: collapsedBodyLines
// diff lines while collapsed (plus a "Click to expand" hint when more were
// cut off), the full diff once expanded, with a "Click to collapse" hint.
func expandableDiffContent(sty *styles.Styles, file, oldContent, newContent string, width int, expanded, hovered bool) string {
	bodyWidth := width - toolBodyLeftPaddingTotal
	if bodyWidth <= 0 {
		return ""
	}

	formatter := common.DiffFormatter(sty).
		Before(file, oldContent).
		After(file, newContent).
		Width(bodyWidth).
		WrapLines(true)
	// Use split view for wide terminals.
	if width > maxTextWidth {
		formatter = formatter.Split()
	}

	lines := strings.Split(strings.TrimRight(formatter.String(), "\n"), "\n")
	maxLines := len(lines)
	if !expanded {
		maxLines = min(collapsedBodyLines, len(lines))
	}

	out := lines[:maxLines]
	switch {
	case !expanded && len(lines) > maxLines:
		truncationStyle := sty.Tool.DiffTruncation
		if hovered {
			truncationStyle = sty.Tool.DiffTruncationHover
		}
		out = append(out, truncationStyle.
			Width(bodyWidth).
			Render(fmt.Sprintf(" Click to expand (%d more lines)", len(lines)-maxLines)))
	case expanded && len(lines) > collapsedBodyLines:
		truncationStyle := sty.Tool.DiffTruncation
		if hovered {
			truncationStyle = sty.Tool.DiffTruncationHover
		}
		out = append(out, truncationStyle.
			Width(bodyWidth).
			Render(" Click to collapse"))
	}

	return sty.Tool.Body.Render(strings.Join(out, "\n"))
}
