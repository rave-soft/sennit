package common

import (
	"cmp"
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/home"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// PrettyPath formats a file path with home directory shortening and applies
// muted styling.
func PrettyPath(t *styles.Styles, path string, width int) string {
	formatted := home.Short(path)
	return t.Sidebar.WorkingDir.Width(width).Render(formatted)
}

// FormatReasoningEffort formats a reasoning effort level for display.
func FormatReasoningEffort(effort string) string {
	if effort == "xhigh" {
		return "X-High"
	}
	return cases.Title(language.English).String(effort)
}

// PlanWindow is one of a subscription's rate-limit windows: how much of it
// is spent, how long it runs, and when it rolls over.
type PlanWindow struct {
	UsedPercent   int
	WindowMinutes int
	ResetsAt      time.Time
}

// FormatPlanUsage renders a one-line summary of a subscription and what is
// left of it, e.g. "Plus · 6% of weekly limit, resets in 3d".
//
// label is the active account's display name, shown right after the plan
// when the provider has more than one account on file (e.g. "Plus ·
// Личный Plus · 42% of weekly limit") — empty for a single-account
// provider, so that case renders exactly as before this parameter existed.
//
// A plan with several windows is summarised by the fullest one: that is the
// one about to run out, and the sidebar has a line, not a table. Windows
// are named by their length rather than by "primary"/"secondary", which
// mean nothing to whoever is reading.
func FormatPlanUsage(plan string, windows []PlanWindow, label string) string {
	var parts []string
	if plan != "" {
		parts = append(parts, cases.Title(language.English).String(plan))
	}
	if label != "" {
		parts = append(parts, label)
	}

	var fullest PlanWindow
	for _, w := range windows {
		if w.WindowMinutes > 0 && w.UsedPercent >= fullest.UsedPercent {
			fullest = w
		}
	}
	if fullest.WindowMinutes > 0 {
		limit := fmt.Sprintf("%d%% of %s limit", fullest.UsedPercent, planWindowName(fullest.WindowMinutes))
		if reset := formatResetIn(fullest.ResetsAt); reset != "" {
			limit += ", resets in " + reset
		}
		parts = append(parts, limit)
	}
	return strings.Join(parts, " · ")
}

// AccountUsageWindows converts an account's stored rate-limit snapshot
// into the shape FormatPlanUsage takes. Mirrors the model package's
// conversion for codex.Usage (see sidebar.go's planWindows) — same shape,
// different type, so there is no shared function to call instead.
func AccountUsageWindows(u workspace.Usage) []PlanWindow {
	var windows []PlanWindow
	for _, w := range []workspace.UsageWindow{u.Primary, u.Secondary} {
		if !w.Known() {
			continue
		}
		windows = append(windows, PlanWindow{
			UsedPercent:   w.UsedPercent,
			WindowMinutes: w.WindowMinutes,
			ResetsAt:      w.ResetsAt,
		})
	}
	return windows
}

// planWindowName names a window by its length, in the words a plan is sold
// in. Anything that is not one of the familiar periods is reported in
// hours, which is still more use than a raw minute count.
func planWindowName(minutes int) string {
	switch {
	case minutes%(60*24*7) == 0 && minutes/(60*24*7) == 1:
		return "weekly"
	case minutes%(60*24) == 0 && minutes/(60*24) == 1:
		return "daily"
	case minutes == 60:
		return "hourly"
	case minutes%(60*24) == 0:
		return fmt.Sprintf("%dd", minutes/(60*24))
	case minutes%60 == 0:
		return fmt.Sprintf("%dh", minutes/60)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

// formatResetIn renders how long until a window rolls over, coarsely: the
// exact minute matters far less than "today" versus "in three days", and a
// long string crowds a narrow sidebar. A reset already in the past reports
// nothing rather than a negative duration.
func formatResetIn(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	d := time.Until(at)
	switch {
	case d <= 0:
		return ""
	case d < time.Hour:
		return fmt.Sprintf("%dm", max(1, int(d.Minutes())))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// ModelContextInfo contains token usage and cost information for a model.
type ModelContextInfo struct {
	ContextUsed    int64
	ModelContext   int64
	Cost           float64
	EstimatedUsage bool
}

// ModelInfo renders model information including name, provider, reasoning
// settings, an optional plan/allowance line, and optional context
// usage/cost.
func ModelInfo(t *styles.Styles, modelName, providerName, reasoningInfo, planInfo string, context *ModelContextInfo, width int) string {
	modelIcon := t.ModelInfo.Icon.Render(styles.ModelIcon)
	modelName = t.ModelInfo.Name.Render(modelName)

	// Build first line with model name and optionally provider on the same line
	var firstLine string
	if providerName != "" {
		providerInfo := t.ModelInfo.Provider.Render(fmt.Sprintf("via %s", providerName))
		modelWithProvider := fmt.Sprintf("%s %s %s", modelIcon, modelName, providerInfo)

		// Check if it fits on one line
		if lipgloss.Width(modelWithProvider) <= width {
			firstLine = modelWithProvider
		} else {
			// If it doesn't fit, put provider on next line
			firstLine = fmt.Sprintf("%s %s", modelIcon, modelName)
		}
	} else {
		firstLine = fmt.Sprintf("%s %s", modelIcon, modelName)
	}

	parts := []string{firstLine}

	// If provider didn't fit on first line, add it as second line
	if providerName != "" && !strings.Contains(firstLine, "via") {
		providerInfo := fmt.Sprintf("via %s", providerName)
		parts = append(parts, t.ModelInfo.ProviderFallback.Render(providerInfo))
	}

	if reasoningInfo != "" {
		parts = append(parts, t.ModelInfo.Reasoning.Render(reasoningInfo))
	}

	// The plan line answers "what am I on, and how much is left" — the two
	// things a subscription hides that a per-token bill would show.
	if planInfo != "" {
		parts = append(parts, t.ModelInfo.Reasoning.Render(planInfo))
	}

	if context != nil {
		formattedInfo := formatTokensAndCost(t, context.ContextUsed, context.ModelContext, context.Cost, context.EstimatedUsage)
		parts = append(parts, lipgloss.NewStyle().PaddingLeft(2).Render(formattedInfo))
	}

	return lipgloss.NewStyle().Width(width).Render(
		lipgloss.JoinVertical(lipgloss.Left, parts...),
	)
}

// formatTokensAndCost formats token usage and cost with appropriate units
// (K/M) and percentage of context window.
func formatTokensAndCost(t *styles.Styles, tokens, contextWindow int64, cost float64, estimated bool) string {
	var formattedTokens string
	switch {
	case tokens >= 1_000_000:
		formattedTokens = fmt.Sprintf("%.1fM", float64(tokens)/1_000_000)
	case tokens >= 1_000:
		formattedTokens = fmt.Sprintf("%.1fK", float64(tokens)/1_000)
	default:
		formattedTokens = fmt.Sprintf("%d", tokens)
	}

	if strings.HasSuffix(formattedTokens, ".0K") {
		formattedTokens = strings.Replace(formattedTokens, ".0K", "K", 1)
	}
	if strings.HasSuffix(formattedTokens, ".0M") {
		formattedTokens = strings.Replace(formattedTokens, ".0M", "M", 1)
	}

	var percentage float64
	if contextWindow > 0 {
		percentage = (float64(tokens) / float64(contextWindow)) * 100
	}

	formattedCost := t.ModelInfo.Cost.Render(fmt.Sprintf("$%.2f", cost))

	formattedTokens = t.ModelInfo.TokenCount.Render(fmt.Sprintf("(%s)", formattedTokens))
	percentageText := fmt.Sprintf("%d%%", int(percentage))
	if estimated {
		percentageText = "~" + percentageText
	}
	formattedPercentage := t.ModelInfo.TokenPercentage.Render(percentageText)
	formattedTokens = fmt.Sprintf("%s %s", formattedPercentage, formattedTokens)
	if percentage > 80 {
		formattedTokens = fmt.Sprintf("%s %s", styles.LSPWarningIcon, formattedTokens)
	}

	return fmt.Sprintf("%s %s", formattedTokens, formattedCost)
}

// StatusOpts defines options for rendering a status line with icon, title,
// description, and optional extra content.
type StatusOpts struct {
	Icon             string // if empty no icon will be shown
	Title            string
	TitleColor       color.Color
	Description      string
	DescriptionColor color.Color
	ExtraContent     string // additional content to append after the description
}

// Status renders a status line with icon, title, description, and extra
// content. The description is truncated if it exceeds the available width.
func Status(t *styles.Styles, opts StatusOpts, width int) string {
	icon := opts.Icon
	title := opts.Title
	description := opts.Description

	titleColor := cmp.Or(opts.TitleColor, t.Resource.DefaultTitleFg)
	descriptionColor := cmp.Or(opts.DescriptionColor, t.Resource.DefaultDescFg)

	title = t.Resource.RowTitleBase.Foreground(titleColor).Render(title)

	if description != "" {
		extraContentWidth := lipgloss.Width(opts.ExtraContent)
		if extraContentWidth > 0 {
			extraContentWidth += 1
		}
		description = ansi.Truncate(description, width-lipgloss.Width(icon)-lipgloss.Width(title)-2-extraContentWidth, "…")
		description = t.Resource.RowDescBase.Foreground(descriptionColor).Render(description)
	}

	var content []string
	if icon != "" {
		content = append(content, icon)
	}
	content = append(content, title)
	if description != "" {
		content = append(content, description)
	}
	if opts.ExtraContent != "" {
		content = append(content, opts.ExtraContent)
	}

	return strings.Join(content, " ")
}

// Section renders a section header with a title and a horizontal line filling
// the remaining width.
func Section(t *styles.Styles, text string, width int, info ...string) string {
	return sectionStyled(t, t.Section.Title, text, width, info...)
}

// SectionStyled is Section with the title style overridable — used where a
// section header needs a state-dependent title style (e.g. a hover cue) but
// must otherwise share Section's title/dashes/info layout.
func SectionStyled(t *styles.Styles, titleStyle lipgloss.Style, text string, width int, info ...string) string {
	return sectionStyled(t, titleStyle, text, width, info...)
}

// sectionStyled is the shared body of Section and SectionStyled: title,
// then a horizontal line filling the remaining width, then optional info
// text right after the line.
func sectionStyled(t *styles.Styles, titleStyle lipgloss.Style, text string, width int, info ...string) string {
	char := styles.SectionSeparator
	length := lipgloss.Width(text) + 1
	remainingWidth := width - length

	var infoText string
	if len(info) > 0 {
		infoText = strings.Join(info, " ")
		if len(infoText) > 0 {
			infoText = " " + infoText
			remainingWidth -= lipgloss.Width(infoText)
		}
	}

	text = titleStyle.Render(text)
	if remainingWidth > 0 {
		text = text + " " + t.Section.Line.Render(strings.Repeat(char, remainingWidth)) + infoText
	}
	return text
}

// DialogTitle renders a dialog title with a decorative line filling the
// remaining width. When the title alone exceeds the available width it is
// truncated with an ellipsis so it never wraps.
func DialogTitle(t *styles.Styles, title string, width int, fromColor, toColor color.Color) string {
	if width > 0 && lipgloss.Width(title) > width {
		return ansi.Truncate(title, width, "…")
	}
	char := "╱"
	length := lipgloss.Width(title) + 1
	remainingWidth := width - length
	if remainingWidth > 0 {
		lines := strings.Repeat(char, remainingWidth)
		lines = styles.ApplyForegroundGrad(t.Dialog.TitleLineBase, lines, fromColor, toColor)
		title = title + " " + lines
	}
	return title
}
