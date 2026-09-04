package chat

import (
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/spin"
	"github.com/rave-soft/sennit/internal/ui/presentation"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// pendingTool renders a tool that is still in progress with an animation,
// and says how much of the call's arguments has arrived when there is
// enough of them for the wait to be about them (see streamingArgsHint).
func pendingTool(sty *styles.Styles, name string, opts *ToolRenderOpts) string {
	return pendingToolLine(sty, name, opts.Anim, opts.Compact, len(opts.ToolCall.Input))
}

// argsStreamingThreshold is how much of a call's arguments has to have
// arrived before the pending line mentions them at all. Most calls carry a
// path or a command and land in one breath; saying "0.2 KB" about those
// would be noise on every tool in the transcript. What this is for is the
// call whose arguments are the work - a write of a file composed in one go
// - where the wait is minutes long and otherwise indistinguishable from a
// hang.
const argsStreamingThreshold = 2 * 1024

// streamingArgsHint renders how much of a pending call's arguments has
// streamed in so far, or nothing while that is still small. It reads a
// partial, unparseable fragment of JSON on purpose: its size is the one
// thing about it that is meaningful mid-stream, and a number that keeps
// growing is what separates a model still writing from one that stopped.
func streamingArgsHint(sty *styles.Styles, streamedArgs int) string {
	if streamedArgs < argsStreamingThreshold {
		return ""
	}
	return sty.Tool.StateWaiting.Render(formatSize(streamedArgs))
}

// pendingToolLine is pendingTool for the callers that do not render straight
// from a [ToolRenderOpts]. streamedArgs is how many bytes of the call's
// arguments have arrived; pass 0 for a caller that has no streaming
// arguments to report.
func pendingToolLine(sty *styles.Styles, name string, anim *spin.Anim, nested bool, streamedArgs int) string {
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

	line := fmt.Sprintf("%s %s %s", icon, toolName, animView)
	if hint := streamingArgsHint(sty, streamedArgs); hint != "" {
		line += " " + hint
	}
	return line
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

// expectedToolRefusals are phrases marking a tool response that reads as an
// error to the API but is ordinary protocol here: the agent is told to read
// the file first, or to re-read one that moved underneath it, and then
// retries. Nothing failed, so these carry the WARN tag rather than the
// destructive-red ERROR one, which is reserved for something actually going
// wrong.
var expectedToolRefusals = []string{
	"User denied permission",
	"User cancelled",
	"has not been read in this session",
	"it changed on disk after you read it",
}

// isExpectedToolRefusal reports whether content is one of the refusals above.
func isExpectedToolRefusal(content string) bool {
	for _, phrase := range expectedToolRefusals {
		if strings.Contains(content, phrase) {
			return true
		}
	}
	return false
}

// shortenPathsToFit fits a one-line error message into width by shrinking the
// file paths inside it rather than cutting the sentence off at the right.
// Paths are home-shortened the way tool headers show them, then the longest
// one is elided from the head — "cannot edit …/chat/tools_render.go: it has
// not been read" says what happened, where "cannot edit /home/user/Proj…"
// says nothing at all. Truncation still happens afterwards if the sentence
// alone overflows; this only keeps the paths from eating the whole budget.
func shortenPathsToFit(msg string, width int) string {
	fields := strings.Split(msg, " ")
	for i, f := range fields {
		if presentation.IsLikelyPath(f) {
			fields[i] = fsext.PrettyPath(f)
		}
	}
	for {
		joined := strings.Join(fields, " ")
		over := ansi.StringWidth(joined) - width
		if over <= 0 {
			return joined
		}
		// Shrink the longest path first, and never past its file name: past
		// that point a path stops identifying anything, so the sentence is
		// the better thing to spend the remaining cells on.
		idx, longest, floor := -1, 0, 0
		for i, f := range fields {
			w := ansi.StringWidth(f)
			if !presentation.IsLikelyPath(f) || w <= longest {
				continue
			}
			if base := ansi.StringWidth(filepath.Base(f)) + 1; w > base {
				idx, longest, floor = i, w, base
			}
		}
		if idx < 0 {
			return joined
		}
		fields[idx] = presentation.TruncatePath(fields[idx], max(floor, longest-over))
		if ansi.StringWidth(fields[idx]) >= longest {
			// No progress to be had from this path; stop rather than spin.
			return strings.Join(fields, " ")
		}
	}
}

// toolErrorContent formats an error message with an ERROR or WARN tag.
func toolErrorContent(sty *styles.Styles, result *message.ToolResult, width int) string {
	if result == nil {
		return ""
	}
	errContent := oneLine(result.Content)
	tag := sty.Tool.ErrorTag.Render("ERROR")
	msgStyle := sty.Tool.ErrorMessage
	if isExpectedToolRefusal(errContent) {
		tag = sty.Tool.WarnTag.Render("WARN")
		msgStyle = sty.Tool.WarnMessage
	}
	tagWidth := lipgloss.Width(tag)
	errContent = shortenPathsToFit(errContent, width-tagWidth-3)
	errContent = ansi.Truncate(errContent, width-tagWidth-3, "…")
	return fmt.Sprintf("%s %s", tag, msgStyle.Render(errContent))
}

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
	// minSpaceForMainParam is the minimum space the main param needs before
	// key=value pairs are shown alongside it; below this, only the main
	// param is shown.
	const minSpaceForMainParam = 30
	if len(params) == 0 {
		return ""
	}

	mainParam := oneLine(params[0])

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
// truncated from the left ("…/chat/tools_render.go") rather than the
// right: the file name is the part being looked for, and a header reading
// "internal/ui/chat/tools_re…" identifies nothing. Anything that is not
// unambiguously a path (a bash command, a prompt, a URL) keeps the
// ordinary right truncation, where the head is what carries the meaning.
func truncateToolParam(s string, width int) string {
	return presentation.TruncatePathAware(s, width)
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

// pagedCountSummary reports a labeled count for a collapsed header, adding
// "of total" when the page shown is smaller than the full result (e.g.
// "100 of 4213 matches") — a plain countSummary would silently hide that a
// grep/glob/ls/read result was cut down to a page, which reads as "that's
// everything" when it isn't. Falls back to countSummary when total is not
// bigger than n (nothing was paged, or the caller has no total to report).
func pagedCountSummary(n, total int, singular, plural string) string {
	if total <= n {
		return countSummary(n, singular, plural)
	}
	if n == 1 {
		return fmt.Sprintf("1 of %d %s", total, plural)
	}
	return fmt.Sprintf("%d of %d %s", n, total, plural)
}

// incompleteSuffix notes, alongside a paged/count summary, that part of the
// underlying tree could not be read at all. It is deliberately a different
// message from the "N of M" paging above: paging means a cursor can fetch
// the rest, Incomplete means some of the tree was silently skipped and no
// cursor will recover it — see proto.GrepResponseMetadata's doc comment.
func incompleteSuffix(summary string, incomplete bool) string {
	if !incomplete {
		return summary
	}
	const note = "some results unreadable"
	if summary == "" {
		return note
	}
	return summary + ", " + note
}
