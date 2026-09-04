package chat

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/stringext"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/presentation"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// toolOutputPlainContent renders plain text, capped to responseContextHeight
// lines. There is no click-to-expand for tool bodies (see
// appendResultSummary) — the only remaining caller of this is
// toolOutputMarkdownContent's parse-error fallback, itself only reachable
// from the still-alive running-delegation preview in agent.go.
func toolOutputPlainContent(sty *styles.Styles, content string, width int) string {
	content = stringext.NormalizeSpace(content)
	content = common.StripCursorControl(content)
	content = common.RemapANSI16(content, sty.ANSI)
	lines := strings.Split(content, "\n")

	maxLines := min(responseContextHeight, len(lines))

	var out []string
	for i, ln := range lines {
		if i >= maxLines {
			break
		}
		ln = " " + ln
		if lipgloss.Width(ln) > width {
			ln = ansi.Truncate(ln, width, "…")
		}
		out = append(out, sty.Tool.ContentLine.Width(width).Render(ln))
	}

	if len(lines) > maxLines {
		out = append(out, sty.Tool.ContentTruncation.
			Width(width).
			Render(fmt.Sprintf(previewTruncateFormat, len(lines)-maxLines)))
	}

	return strings.Join(out, "\n")
}

// toolOutputImageContent renders image data with size info.
func toolOutputImageContent(sty *styles.Styles, data, mediaType string) string {
	dataSize := len(data) * 3 / 4
	sizeStr := formatSize(dataSize)

	return sty.Tool.Body.Render(fmt.Sprintf(
		"%s %s %s %s",
		sty.Tool.ResourceLoadedText.Render("Loaded Image"),
		sty.Tool.ResourceLoadedIndicator.Render(styles.ArrowRightIcon),
		sty.Tool.MediaType.Render(mediaType),
		sty.Tool.ResourceSize.Render(sizeStr),
	))
}

// toolOutputSkillContent renders a skill loaded indicator.
func toolOutputSkillContent(sty *styles.Styles, name, description string) string {
	return sty.Tool.Body.Render(fmt.Sprintf(
		"%s %s %s %s",
		sty.Tool.ResourceLoadedText.Render("Loaded Skill"),
		sty.Tool.ResourceLoadedIndicator.Render(styles.ArrowRightIcon),
		sty.Tool.ResourceName.Render(name),
		sty.Tool.ResourceSize.Render(description),
	))
}

// toolOutputHookIndicator renders hook indicator lines from tool metadata.
// Returns empty string if no hook metadata is present. Every per-hook
// column — name, matcher, and detail (a Decision plus, on deny, the
// hook's raw stderr as Reason) — is sanitized to one line and truncated to
// fit the available horizontal space; Reason in particular can be a
// multi-line lint/script output (see hooks/runner.go), so leaving it
// unsanitized would blow the indicator into several lines and, on a narrow
// terminal, off the right edge entirely.
func toolOutputHookIndicator(sty *styles.Styles, metadata string, width int) string {
	if metadata == "" {
		return ""
	}
	var meta struct {
		Hook *proto.HookMetadata `json:"hook"`
	}
	if err := json.Unmarshal([]byte(metadata), &meta); err != nil || meta.Hook == nil {
		return ""
	}
	h := meta.Hook
	if len(h.Hooks) == 0 {
		return ""
	}

	// Sanitize every column to one line and compute max widths for the
	// name, matcher, and detail columns so they align. The name column is
	// capped at maxHookNameWidth characters.
	const maxHookNameWidth = 30
	sanitizedNames := make([]string, len(h.Hooks))
	sanitizedMatchers := make([]string, len(h.Hooks))
	details := make([]string, len(h.Hooks))
	maxNameWidth := 0
	maxMatcherWidth := 0
	maxDetailWidth := 0
	for i, hi := range h.Hooks {
		sanitizedNames[i] = sanitizeHookText(hi.Name)
		w := lipgloss.Width(sty.Tool.HookName.Render(sanitizedNames[i]))
		if w > maxNameWidth {
			maxNameWidth = w
		}
		sanitizedMatchers[i] = sanitizeHookText(hi.Matcher)
		if sanitizedMatchers[i] != "" {
			mw := lipgloss.Width(sty.Tool.HookMatcher.Render(sanitizedMatchers[i]))
			if mw > maxMatcherWidth {
				maxMatcherWidth = mw
			}
		}
		details[i] = hookDetail(sty, hi, sanitizeHookText(hi.Reason))
		if dw := lipgloss.Width(details[i]); dw > maxDetailWidth {
			maxDetailWidth = dw
		}
	}

	if maxNameWidth > maxHookNameWidth {
		maxNameWidth = maxHookNameWidth
	}

	// Cap the name column so the widest line still fits in width. The
	// per-line layout is:
	//   "Hook " + name(padded) + [" " + matcher(padded)] + " → " + detail
	detailBudget := maxDetailWidth
	if width > 0 {
		fixed := lipgloss.Width(sty.Tool.HookLabel.Render("Hook")) + 1
		if maxMatcherWidth > 0 {
			fixed += 1 + maxMatcherWidth
		}
		fixed += 1 + lipgloss.Width(sty.Tool.HookArrow.Render(styles.ArrowRightIcon)) + 1
		if budget := width - fixed - maxDetailWidth; budget < maxNameWidth {
			maxNameWidth = max(1, budget)
		}
		// The name column absorbed what it could; whatever's left after
		// the (now possibly shrunk) fixed columns is the detail's actual
		// budget. Unlike the name column, a too-long detail was
		// previously never truncated at all — maxDetailWidth was computed
		// and then ignored, so a hook's raw stderr line could run the
		// indicator past the terminal edge.
		detailBudget = max(1, width-fixed-maxNameWidth)
	}

	var lines []string
	for i, hi := range h.Hooks {
		name := truncateHookName(sanitizedNames[i], maxNameWidth)
		detail := ansi.Truncate(details[i], detailBudget, "…")
		lines = append(lines, renderHookLine(sty, hi, sanitizedMatchers[i], name, detail, maxNameWidth, maxMatcherWidth))
	}
	return strings.Join(lines, "\n")
}

// sanitizeHookText collapses a hook-supplied string to one line: embedded
// newlines become ¶ (kept, rather than dropped, so a multi-line message —
// most commonly a blocking hook's raw stderr, see hooks/runner.go — still
// hints that there was more) and any other whitespace run collapses to a
// single space.
func sanitizeHookText(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", "¶")), " ")
}

// truncateHookName truncates a hook name to fit within maxWidth cells:
// from the left for a path (`…/format.sh`), from the right for anything
// else — the same rule tool headers and the sidebar's file list follow.
func truncateHookName(name string, maxWidth int) string {
	return presentation.TruncatePathAware(name, maxWidth)
}

// renderHookLine renders a single hook indicator line with aligned columns.
// matcher is already sanitized to one line (see sanitizeHookText) and
// detail is already truncated to its column budget.
func renderHookLine(sty *styles.Styles, hi proto.HookInfo, matcher, rawName, detail string, maxNameWidth, maxMatcherWidth int) string {
	name := sty.Tool.HookName.Render(rawName)
	namePad := strings.Repeat(" ", max(0, maxNameWidth-lipgloss.Width(name)))

	var matcherPart string
	if maxMatcherWidth > 0 {
		if matcher != "" {
			styledMatcher := sty.Tool.HookMatcher.Render(matcher)
			matcherPad := strings.Repeat(" ", max(0, maxMatcherWidth-lipgloss.Width(styledMatcher)))
			matcherPart = " " + styledMatcher + matcherPad
		} else {
			matcherPart = " " + strings.Repeat(" ", maxMatcherWidth)
		}
	}

	labelStyle := sty.Tool.HookLabel
	arrowStyle := sty.Tool.HookArrow
	if hi.Decision == "deny" {
		labelStyle = sty.Tool.HookDeniedLabel
		arrowStyle = sty.Tool.HookDeniedLabel
	}

	return fmt.Sprintf(
		"%s %s%s%s %s %s",
		labelStyle.Render("Hook"),
		name,
		namePad,
		matcherPart,
		arrowStyle.Render(styles.ArrowRightIcon),
		detail,
	)
}

// hookDetail returns the styled detail text for a single hook result.
// reason is already sanitized to one line (see sanitizeHookText).
func hookDetail(sty *styles.Styles, hi proto.HookInfo, reason string) string {
	const (
		okMessage     = "OK"
		denialMessage = "Denied"
		// haltedMessage marks a hook that stopped the whole agent turn
		// (hooks.HaltExitCode, see runner.go), not just this one tool
		// call — a materially bigger consequence than an ordinary deny,
		// so it gets its own word rather than reading as "Denied" like
		// any other blocked call.
		haltedMessage  = "Halted"
		rewroteMessage = "Rewrote Output"
	)
	switch hi.Decision {
	case "deny":
		label := denialMessage
		if hi.Halt {
			label = haltedMessage
		}
		if reason != "" {
			return sty.Tool.HookDenied.Render(label) + " " + sty.Tool.HookDeniedReason.Render(reason)
		}
		return sty.Tool.HookDenied.Render(label)
	case "allow":
		result := sty.Tool.HookOK.Render(okMessage)
		if hi.InputRewrite {
			result += " " + sty.Tool.HookRewrote.Render(rewroteMessage)
		}
		return result
	default:
		result := sty.Tool.HookOK.Render(okMessage)
		if hi.InputRewrite {
			result += " " + sty.Tool.HookRewrote.Render(rewroteMessage)
		}
		return result
	}
}

// formatSize formats byte size into human readable format.
func formatSize(bytes int) string {
	const (
		kb = 1024
		mb = kb * 1024
	)
	switch {
	case bytes >= mb:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(kb))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// formatTimeout converts timeout seconds to a duration string (e.g., "30s").
// Returns empty string if timeout is 0.
func formatTimeout(timeout int) string {
	if timeout == 0 {
		return ""
	}
	return fmt.Sprintf("%ds", timeout)
}
