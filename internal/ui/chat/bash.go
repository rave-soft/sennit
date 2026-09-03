package chat

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/message"
	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/stringext"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/presentation"
	"github.com/rave-soft/sennit/internal/ui/styles"
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

// registerBashToolRenderers registers the bash and job tool renderers.
func registerBashToolRenderers() {
	registerToolItemFactory(tools.BashToolName,
		func(sty *styles.Styles, tc message.ToolCall, res *message.ToolResult, canceled bool) ToolMessageItem {
			return NewBashToolMessageItem(sty, tc, res, canceled)
		},
		&BashToolRenderContext{})
	registerToolItemFactory(tools.JobOutputToolName,
		func(sty *styles.Styles, tc message.ToolCall, res *message.ToolResult, canceled bool) ToolMessageItem {
			return NewJobOutputToolMessageItem(sty, tc, res, canceled)
		},
		&JobOutputToolRenderContext{})
	registerToolRenderer(tools.JobKillToolName, &JobKillToolRenderContext{})
}

// BashToolRenderContext renders bash tool messages.
type BashToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (b *BashToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	if opts.IsPending() {
		return pendingTool(sty, "Bash", opts)
	}

	var params tools.BashParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		params.Command = "failed to parse command"
	}

	var meta tools.BashResponseMetadata
	if opts.HasResult() {
		_ = json.Unmarshal([]byte(opts.Result.Metadata), &meta)
	}

	if meta.Background {
		// The result's own text is an instruction to the model ("use
		// job_output to view output"), not news; what a reader wants
		// from a job that just started is what it is and what it runs.
		description := cleanDescription(meta.Description)
		body := params.Command
		if description == "" {
			description, body = params.Command, ""
		}
		return renderJobTool(sty, opts, width, jobLine{
			shellID: meta.ShellID,
			subject: description,
			summary: "started",
			body:    body,
		})
	}

	// Regular bash command.
	cmd := strings.ReplaceAll(params.Command, "\n", " ")
	cmd = strings.ReplaceAll(cmd, "\t", "    ")
	toolParams := []string{cmd}
	if params.RunInBackground {
		toolParams = append(toolParams, "background", "true")
	}

	header := toolHeader(sty, opts.Status, "Bash", width, opts, toolParams...)
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
		if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
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
	return header + "\n" + expandableBodyContent(sty, output, width, opts.Expanded, opts.Hovered)
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
func expandableBodyContent(sty *styles.Styles, content string, width int, expanded, hovered bool) string {
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
	lineStyle := sty.Tool.ContentLine
	for _, ln := range lines[:maxLines] {
		ln = " " + ln
		if lipgloss.Width(ln) > width {
			ln = ansi.Truncate(ln, width, "…")
		}
		out = append(out, lineStyle.Width(width).Render(ln))
	}

	switch {
	case !expanded && len(lines) > maxLines:
		truncationStyle := sty.Tool.ContentTruncation
		if hovered {
			truncationStyle = sty.Tool.ContentTruncationHover
		}
		out = append(out, truncationStyle.
			Width(width).
			Render(fmt.Sprintf(" Click to expand (%d more lines)", len(lines)-maxLines)))
	case expanded && len(lines) > collapsedBodyLines:
		truncationStyle := sty.Tool.ContentTruncation
		if hovered {
			truncationStyle = sty.Tool.ContentTruncationHover
		}
		out = append(out, truncationStyle.
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
	return presentation.FormatElapsed(time.Duration(meta.EndTime-meta.StartTime) * time.Millisecond)
}

// -----------------------------------------------------------------------------
// Job Output Tool
// -----------------------------------------------------------------------------

// JobOutputToolMessageItem is a job_output call, whose body — the job's
// own output — expands on a click exactly like a bash command's.
type JobOutputToolMessageItem struct {
	*baseToolMessageItem
}

var (
	_ ToolMessageItem = (*JobOutputToolMessageItem)(nil)
	_ Expandable      = (*JobOutputToolMessageItem)(nil)
)

// NewJobOutputToolMessageItem creates a new [JobOutputToolMessageItem].
func NewJobOutputToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return &JobOutputToolMessageItem{newBaseToolMessageItem(sty, toolCall, result, &JobOutputToolRenderContext{}, canceled)}
}

// ToggleExpanded implements Expandable: a click on a finished job_output
// call toggles between the collapsed output preview and the full output.
func (j *JobOutputToolMessageItem) ToggleExpanded() bool {
	return j.toggleExpanded()
}

// JobOutputToolRenderContext renders job_output tool messages.
type JobOutputToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (j *JobOutputToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	if opts.IsPending() {
		return pendingTool(sty, "Job", opts)
	}

	var params tools.JobOutputParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, width)
	}

	var meta tools.JobOutputResponseMetadata
	if opts.HasResult() && opts.Result.Metadata != "" {
		_ = json.Unmarshal([]byte(opts.Result.Metadata), &meta)
	}

	content := ""
	if opts.HasResult() {
		content = opts.Result.Content
	}
	return renderJobTool(sty, opts, width, jobLine{
		shellID: params.ShellID,
		subject: cmp.Or(cleanDescription(meta.Description), meta.Command),
		summary: jobOutcomeSummary(meta),
		body:    jobOutputBody(meta, content),
	})
}

// jobOutcomeSummary says what became of a background job in the two or
// three words a header line has room for: still running, finished, or
// finished badly and with which exit code.
//
// A result stored before this metadata carried a status still reports
// running/done off its Done flag; only the exit code is beyond reach
// there, and an exit code guessed would be worse than one missing.
func jobOutcomeSummary(meta tools.JobOutputResponseMetadata) string {
	if meta.Status == "" && !meta.Done {
		return "running"
	}
	if !meta.Done {
		return meta.Status
	}
	if meta.ExitCode != 0 {
		return fmt.Sprintf("exit %d", meta.ExitCode)
	}
	return "done"
}

// jobOutputBody is the job's own output, with the status preamble the
// model reads ("Status: running\n\n…") left out of the transcript — the
// header already says that, in fewer words.
func jobOutputBody(meta tools.JobOutputResponseMetadata, content string) string {
	if meta.Output != "" {
		return meta.Output
	}
	// A result stored before Output was recorded: recover the body by
	// dropping the preamble the tool wrote in front of it.
	if _, rest, ok := strings.Cut(content, "\n\n"); ok && strings.HasPrefix(content, "Status: ") {
		content = rest
	}
	if content == tools.BashNoOutput {
		return ""
	}
	return content
}

// -----------------------------------------------------------------------------
// Job Kill Tool
// -----------------------------------------------------------------------------

// JobKillToolRenderContext renders job_kill tool messages.
type JobKillToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (j *JobKillToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	if opts.IsPending() {
		return pendingTool(sty, "Job", opts)
	}

	var params tools.JobKillParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, width)
	}

	var description string
	if opts.HasResult() && opts.Result.Metadata != "" {
		var meta tools.JobKillResponseMetadata
		if err := json.Unmarshal([]byte(opts.Result.Metadata), &meta); err == nil {
			description = cmp.Or(cleanDescription(meta.Description), meta.Command)
		}
	}

	return renderJobTool(sty, opts, width, jobLine{
		shellID: params.ShellID,
		subject: description,
		summary: "killed",
	})
}

// jobLine is what one job_* line in the transcript is made of: which job
// (subject, falling back to its shell id when nothing better is known),
// what became of it (summary), and whatever output is worth showing under
// the header (body).
//
// Subject leads and the shell id is only a fallback: a shell id addresses
// the job to the model, which has just read it out of a tool result, but
// to a reader it is a number that appears nowhere else.
type jobLine struct {
	shellID string
	subject string
	summary string
	body    string
}

// renderJobTool renders a job-related tool with the common pattern:
// header → early state → body.
func renderJobTool(sty *styles.Styles, opts *ToolRenderOpts, width int, line jobLine) string {
	header := jobHeader(sty, opts.Status, line, width)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
		return joinToolParts(header, earlyState)
	}

	header = appendResultSummary(sty, header, line.summary)
	if line.body == "" {
		return header
	}
	// No line count in the header: the body's own truncation notice
	// already says how much of it is not on screen.
	return header + "\n" + expandableBodyContent(sty, line.body, width, opts.Expanded, opts.Hovered)
}

// jobHeader builds a header for job-related tools.
// Format: "● Job <what the job is doing>".
func jobHeader(sty *styles.Styles, status ToolStatus, line jobLine, width int) string {
	prefix := toolIcon(sty, status) + " " + sty.Tool.JobToolName.Render("Job")

	subject, subjectStyle := line.subject, sty.Tool.JobDescription
	if subject == "" && line.shellID != "" {
		subject, subjectStyle = "PID "+line.shellID, sty.Tool.JobPID
	}
	if subject == "" {
		return prefix
	}

	availableWidth := width - lipgloss.Width(prefix) - 1
	if availableWidth < 10 {
		return prefix
	}
	return prefix + " " + subjectStyle.Render(ansi.Truncate(subject, availableWidth, "…"))
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
