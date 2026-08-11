package chat

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/stringext"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// -----------------------------------------------------------------------------
// Bash Tool
// -----------------------------------------------------------------------------

// BashToolMessageItem is a message item that represents a bash tool call.
type BashToolMessageItem struct {
	*baseToolMessageItem
}

var (
	_ ToolMessageItem = (*BashToolMessageItem)(nil)
	_ Expandable      = (*BashToolMessageItem)(nil)
)

// NewBashToolMessageItem creates a new [BashToolMessageItem].
func NewBashToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return &BashToolMessageItem{newBaseToolMessageItem(sty, toolCall, result, &BashToolRenderContext{}, canceled)}
}

// BashToolRenderContext renders bash tool messages.
type BashToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (b *BashToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, "Bash", opts.Anim, opts.Compact)
	}

	var params tools.BashParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		params.Command = "failed to parse command"
	}

	// Check if this is a background job.
	var meta tools.BashResponseMetadata
	if opts.HasResult() {
		_ = json.Unmarshal([]byte(opts.Result.Metadata), &meta)
	}

	if meta.Background {
		description := cmp.Or(cleanDescription(meta.Description), params.Command)
		content := "Command: " + params.Command + "\n" + opts.Result.Content
		return renderJobTool(sty, opts, cappedWidth, "Start", meta.ShellID, description, content)
	}

	// Regular bash command.
	cmd := strings.ReplaceAll(params.Command, "\n", " ")
	cmd = strings.ReplaceAll(cmd, "\t", "    ")
	toolParams := []string{cmd}
	if params.RunInBackground {
		toolParams = append(toolParams, "background", "true")
	}

	header := toolHeader(sty, opts.Status, "Bash", cappedWidth, opts, toolParams...)
	if opts.Compact {
		return header
	}

	// opts.IsPending() (and the spinner) only cover the window while the
	// model is still streaming the command's input. tc.Finished flips true
	// as soon as that JSON is complete — for anything that takes real time
	// to run (npm install, a build, ...) there is then a second window,
	// often much longer, where the command is still executing and no
	// result has arrived yet. toolEarlyStateContent's Running case exists
	// for other tools that legitimately show a status line there, but for
	// Bash it appended a dangling "Waiting for tool response..." line
	// below the header. Treat that window the same as the header-only
	// pending state instead.
	if opts.Status != ToolStatusRunning {
		if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
			return joinToolParts(header, earlyState)
		}
	}

	if !opts.HasResult() {
		return header
	}

	output := meta.Output
	if output == "" && opts.Result.Content != tools.BashNoOutput {
		output = opts.Result.Content
	}
	if output == "" {
		return header
	}

	header = appendResultSummary(sty, header, bashDurationSummary(meta))
	// The body sits directly under the header (no blank separator line):
	// up to collapsedBodyLines lines by default, the full output
	// once the item is click-expanded (see BashToolMessageItem.ToggleExpanded).
	return header + "\n" + expandableBodyContent(sty, output, cappedWidth, opts.Expanded)
}

// collapsedBodyLines is how many content lines a collapsed expandable tool
// body (bash output, written file content) shows under its header before
// offering click-to-expand.
const collapsedBodyLines = 4

// expandableBodyContent renders a tool's content body under its header —
// shared by Bash (command output) and Write (file content). While collapsed
// it shows at most collapsedBodyLines lines followed by a "Click to expand"
// hint when more were cut off; expanded it shows the full content with a
// "Click to collapse" hint at the end.
func expandableBodyContent(sty *styles.Styles, content string, width int, expanded bool) string {
	content = stringext.NormalizeSpace(content)
	content = common.StripCursorControl(content)
	// Drop the command's own ANSI colors (red test failures, linter
	// output, ...) entirely so the body renders in the uniform
	// ContentLine color instead of whatever the command printed.
	content = ansi.Strip(content)
	lines := strings.Split(content, "\n")

	maxLines := len(lines)
	if !expanded {
		maxLines = min(collapsedBodyLines, len(lines))
	}

	out := make([]string, 0, maxLines+1)
	for _, ln := range lines[:maxLines] {
		ln = " " + ln
		if lipgloss.Width(ln) > width {
			ln = ansi.Truncate(ln, width, "…")
		}
		out = append(out, sty.Tool.ContentLine.Width(width).Render(ln))
	}

	switch {
	case !expanded && len(lines) > maxLines:
		out = append(out, sty.Tool.ContentTruncation.
			Width(width).
			Render(fmt.Sprintf(" Click to expand (%d more lines)", len(lines)-maxLines)))
	case expanded && len(lines) > collapsedBodyLines:
		out = append(out, sty.Tool.ContentTruncation.
			Width(width).
			Render(" Click to collapse"))
	}

	return strings.Join(out, "\n")
}

// ToggleExpanded implements Expandable: a click on a finished bash call
// toggles between the collapsed 4-line output preview and the full output.
func (b *BashToolMessageItem) ToggleExpanded() bool {
	return b.toggleExpanded()
}

// bashDurationSummary renders a short "2.1s" style summary for a finished
// bash call's collapsed header. Returns "" when timing wasn't recorded.
func bashDurationSummary(meta tools.BashResponseMetadata) string {
	if meta.StartTime <= 0 || meta.EndTime <= meta.StartTime {
		return ""
	}
	return formatElapsed(time.Duration(meta.EndTime-meta.StartTime) * time.Millisecond)
}

// -----------------------------------------------------------------------------
// Job Output Tool
// -----------------------------------------------------------------------------

// JobOutputToolMessageItem is a message item for job_output tool calls.
type JobOutputToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*JobOutputToolMessageItem)(nil)

// NewJobOutputToolMessageItem creates a new [JobOutputToolMessageItem].
func NewJobOutputToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &JobOutputToolRenderContext{}, canceled)
}

// JobOutputToolRenderContext renders job_output tool messages.
type JobOutputToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (j *JobOutputToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, "Job", opts.Anim, opts.Compact)
	}

	var params tools.JobOutputParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, cappedWidth)
	}

	var description string
	if opts.HasResult() && opts.Result.Metadata != "" {
		var meta tools.JobOutputResponseMetadata
		if err := json.Unmarshal([]byte(opts.Result.Metadata), &meta); err == nil {
			description = cmp.Or(cleanDescription(meta.Description), meta.Command)
		}
	}

	content := ""
	if opts.HasResult() {
		content = opts.Result.Content
	}
	return renderJobTool(sty, opts, cappedWidth, "Output", params.ShellID, description, content)
}

// -----------------------------------------------------------------------------
// Job Kill Tool
// -----------------------------------------------------------------------------

// JobKillToolMessageItem is a message item for job_kill tool calls.
type JobKillToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*JobKillToolMessageItem)(nil)

// NewJobKillToolMessageItem creates a new [JobKillToolMessageItem].
func NewJobKillToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &JobKillToolRenderContext{}, canceled)
}

// JobKillToolRenderContext renders job_kill tool messages.
type JobKillToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (j *JobKillToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, "Job", opts.Anim, opts.Compact)
	}

	var params tools.JobKillParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, cappedWidth)
	}

	var description string
	if opts.HasResult() && opts.Result.Metadata != "" {
		var meta tools.JobKillResponseMetadata
		if err := json.Unmarshal([]byte(opts.Result.Metadata), &meta); err == nil {
			description = cmp.Or(cleanDescription(meta.Description), meta.Command)
		}
	}

	content := ""
	if opts.HasResult() {
		content = opts.Result.Content
	}
	return renderJobTool(sty, opts, cappedWidth, "Kill", params.ShellID, description, content)
}

// renderJobTool renders a job-related tool with the common pattern:
// header → nested check → early state → body.
func renderJobTool(sty *styles.Styles, opts *ToolRenderOpts, width int, action, shellID, description, content string) string {
	header := jobHeader(sty, opts.Status, action, shellID, description, width)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
		return joinToolParts(header, earlyState)
	}

	if content == "" {
		return header
	}

	return appendResultSummary(sty, header, lineCountSummary(content))
}

// jobHeader builds a header for job-related tools.
// Format: "● Job (Action) PID shellID description..."
func jobHeader(sty *styles.Styles, status ToolStatus, action, shellID, description string, width int) string {
	icon := toolIcon(sty, status)
	jobPart := sty.Tool.JobToolName.Render("Job")
	actionPart := sty.Tool.JobAction.Render("(" + action + ")")
	pidPart := sty.Tool.JobPID.Render("PID " + shellID)

	prefix := fmt.Sprintf("%s %s %s %s", icon, jobPart, actionPart, pidPart)

	if description == "" {
		return prefix
	}

	prefixWidth := lipgloss.Width(prefix)
	availableWidth := width - prefixWidth - 1
	if availableWidth < 10 {
		return prefix
	}

	truncatedDesc := ansi.Truncate(description, availableWidth, "…")
	return prefix + " " + sty.Tool.JobDescription.Render(truncatedDesc)
}

// joinToolParts joins header and body with a blank line separator. An empty
// body (the collapsed default — see appendResultSummary) collapses to just
// the header instead of leaving a dangling blank line.
func joinToolParts(header, body string) string {
	if body == "" {
		return header
	}
	return strings.Join([]string{header, "", body}, "\n")
}
