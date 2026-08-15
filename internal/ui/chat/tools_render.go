package chat

import (
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/ui/anim"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// pendingTool renders a tool that is still in progress with an animation.
func pendingTool(sty *styles.Styles, name string, anim *anim.Anim, nested bool) string {
	icon := sty.Tool.IconPending.Render()
	nameStyle := sty.Tool.NameNormal
	if nested {
		nameStyle = sty.Tool.NameNested
	}
	toolName := nameStyle.Render(name)

	var animView string
	if anim != nil {
		animView = anim.Render()
	}

	return fmt.Sprintf("%s %s %s", icon, toolName, animView)
}

// toolEarlyStateContent handles error/cancelled/pending states before content rendering.
// Returns the rendered output and true if early state was handled.
func toolEarlyStateContent(sty *styles.Styles, opts *ToolRenderOpts, width int) (string, bool) {
	var msg string
	switch opts.Status {
	case ToolStatusError:
		msg = toolErrorContent(sty, opts.Result, width)
	case ToolStatusCanceled:
		msg = sty.Tool.StateCancelled.Render("Canceled.")
	case ToolStatusAwaitingPermission:
		msg = sty.Tool.StateWaiting.Render("Requesting permission...")
	case ToolStatusRunning:
		msg = sty.Tool.StateWaiting.Render("Waiting for tool response...")
	default:
		return "", false
	}
	return msg, true
}

// toolErrorContent formats an error message with an ERROR or WARN tag.
func toolErrorContent(sty *styles.Styles, result *message.ToolResult, width int) string {
	if result == nil {
		return ""
	}
	errContent := strings.ReplaceAll(result.Content, "\n", " ")
	if strings.Contains(errContent, "User denied permission") ||
		strings.Contains(errContent, "User cancelled") {
		deniedTag := sty.Tool.WarnTag.Render("WARN")
		deniedTagWidth := lipgloss.Width(deniedTag)
		errContent = ansi.Truncate(errContent, width-deniedTagWidth-3, "…")
		return fmt.Sprintf("%s %s", deniedTag, sty.Tool.WarnMessage.Render(errContent))
	}
	errTag := sty.Tool.ErrorTag.Render("ERROR")
	tagWidth := lipgloss.Width(errTag)
	errContent = ansi.Truncate(errContent, width-tagWidth-3, "…")
	return fmt.Sprintf("%s %s", errTag, sty.Tool.ErrorMessage.Render(errContent))
}

// toolIcon returns the status icon for a tool call.
// toolIcon returns the status icon for a tool call based on its status.
func toolIcon(sty *styles.Styles, status ToolStatus) string {
	switch status {
	case ToolStatusSuccess:
		return sty.Tool.IconSuccess.String()
	case ToolStatusError:
		return sty.Tool.IconError.String()
	case ToolStatusCanceled:
		return sty.Tool.IconCancelled.String()
	default:
		return sty.Tool.IconPending.String()
	}
}

// oneLine collapses any run of whitespace — embedded newlines, tabs, CRLF,
// repeated spaces — into a single space, and trims the ends. Tool params
// routinely carry multi-line values (a bash command built from a
// heredoc/multi-line script, a description, an MCP argument) that a caller
// didn't already flatten; toolParamList applies this to every param before
// truncating so a header stays provably one line — ansi.Truncate only
// prevents *horizontal* overflow, it has no idea embedded "\n"s exist.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// toolParamList formats tool parameters as "main (key=value, ...)",
// truncated to width — a tool header is always a single line, so params
// never wrap.
func toolParamList(sty *styles.Styles, params []string, width int) string {
	// minSpaceForMainParam is the min space required for the main param
	// if this is less that the value set we will only show the main param nothing else
	const minSpaceForMainParam = 30
	if len(params) == 0 {
		return ""
	}

	mainParam := oneLine(params[0])

	// Build key=value pairs from remaining params (consecutive key, value pairs).
	var kvPairs []string
	for i := 1; i+1 < len(params); i += 2 {
		if params[i+1] != "" {
			kvPairs = append(kvPairs, fmt.Sprintf("%s=%s", params[i], oneLine(params[i+1])))
		}
	}

	// Try to include key=value pairs if there's enough space. The main
	// param is fitted to its own budget first, before the pairs are
	// appended: truncating the composed string afterwards would eat the
	// pairs off the end, and for a path main param it is the head, not
	// the tail, that has to go — see truncateToolParam.
	output := truncateToolParam(mainParam, width)
	if len(kvPairs) > 0 {
		partsStr := strings.Join(kvPairs, ", ")
		if remaining := width - lipgloss.Width(partsStr) - 3; remaining >= minSpaceForMainParam {
			output = fmt.Sprintf("%s (%s)", truncateToolParam(mainParam, remaining), partsStr)
		}
	}

	if width >= 0 {
		output = ansi.Truncate(output, width, "…")
	}
	return sty.Tool.ParamMain.Render(output)
}

// truncateToolParam fits a tool header's main param into width. A path is
// truncated from the left ("…/internal/ui/chat/tools_render.go") rather
// than the right: the file name is the part being looked for, and a header
// reading "internal/ui/chat/tools_re…" identifies nothing. Anything that
// is not unambiguously a path (a bash command, a prompt, a URL) keeps the
// ordinary right truncation, where the head is what carries the meaning.
//
// When even the file name alone does not fit, it is truncated on the right
// as a last resort — half a name still beats a bare ellipsis.
func truncateToolParam(s string, width int) string {
	if width < 0 || ansi.StringWidth(s) <= width {
		return s
	}
	if !isLikelyPath(s) {
		return ansi.Truncate(s, width, "…")
	}

	base := filepath.Base(s)
	// One cell for the leading "…"; if the name cannot fit beside it,
	// there is nothing left to preserve.
	if ansi.StringWidth(base)+1 > width {
		return ansi.Truncate(base, width, "…")
	}
	// ansi.TruncateLeft removes n graphemes from the start; pick n so the
	// result plus the "…" prefix fits exactly in width.
	n := ansi.StringWidth(s) - width + 1
	return ansi.TruncateLeft(s, n, "…")
}

// toolHeader builds the tool header line: "● ToolName params...". Always a
// single line — long parameters truncate, never wrap.
func toolHeader(sty *styles.Styles, status ToolStatus, name string, width int, opts *ToolRenderOpts, params ...string) string {
	nested := opts != nil && opts.Compact
	icon := toolIcon(sty, status)
	nameStyle := sty.Tool.NameNormal
	if nested {
		nameStyle = sty.Tool.NameNested
	}
	toolName := nameStyle.Render(name)
	prefix := fmt.Sprintf("%s %s ", icon, toolName)
	remainingWidth := width - lipgloss.Width(prefix)
	return prefix + toolParamList(sty, params, remainingWidth)
}

// junkPlaceholders lists values that mean "nothing" in a model's own
// vocabulary rather than this codebase's — a model filling an optional
// field (most commonly a "description" param) with the literal text
// "None"/"null"/"n/a" instead of leaving it empty is a well-known
// artifact. isJunkText/cleanDescription treat these exactly like "": a
// summary/description this hollow is worse than no summary at all, since
// it reads as real data.
var junkPlaceholders = map[string]bool{
	"none": true, "null": true, "nil": true, "n/a": true, "na": true,
	"undefined": true, "-": true,
}

// isJunkText reports whether s is empty or one of junkPlaceholders
// (case-insensitively, ignoring surrounding whitespace).
func isJunkText(s string) bool {
	return junkPlaceholders[strings.ToLower(strings.TrimSpace(s))]
}

// cleanDescription returns s, or "" if s is a junk placeholder (see
// isJunkText) — for use with cmp.Or(cleanDescription(modelSupplied),
// fallback) so a placeholder value falls through to the fallback instead
// of being displayed as if it were a real description.
func cleanDescription(s string) string {
	if isJunkText(s) {
		return ""
	}
	return s
}

// appendResultSummary appends a short " · outcome" suffix to a collapsed
// tool's header line — e.g. "342 lines", "+12 −3", "27 matches" — so the
// single default line still says what happened without showing any
// content. Returns header unchanged when summary is "" or a junk
// placeholder (see isJunkText) — never prints a value that means nothing.
func appendResultSummary(sty *styles.Styles, header, summary string) string {
	if summary == "" || isJunkText(summary) {
		return header
	}
	return header + " " + sty.Tool.TodoStatusNote.Render("· "+summary)
}

// lineCountSummary reports content's line count for a collapsed header,
// e.g. "342 lines". Returns "" for empty content.
func lineCountSummary(content string) string {
	if content == "" {
		return ""
	}
	n := strings.Count(content, "\n") + 1
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
}

// diffSummary reports a +additions/−removals count for a collapsed diff
// header, e.g. "+12 −3". Returns "" when there's nothing to report.
func diffSummary(additions, removals int) string {
	if additions == 0 && removals == 0 {
		return ""
	}
	return fmt.Sprintf("+%d −%d", additions, removals)
}

// countSummary reports a labeled count for a collapsed header, e.g.
// "27 matches" or "4 files". Returns "" for a non-positive count.
func countSummary(n int, singular, plural string) string {
	if n <= 0 {
		return ""
	}
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}
