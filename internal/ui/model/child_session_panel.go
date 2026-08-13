package model

import (
	"fmt"
	"strings"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/presentation"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// childSessionPanelHeight is the fixed height of the child-session info
// panel that replaces the editor while a sub-agent session is being
// viewed (see drawChildSessionPanel): breadcrumb + name + back button,
// model/effort, token usage + elapsed/duration.
const childSessionPanelHeight = 3

// childPanelButtonLabel is the explicit, clickable "go back up" affordance
// on the child-session panel (see drawChildSessionPanel).
func (m *UI) childPanelButtonLabel() string {
	return "↑ back (" + m.exitChildSessionShortcut() + ")"
}

// delegationInfo resolves a delegation tool item's display name,
// model/effort override, and timing (see chat.DelegationInfoProvider) for
// capture into a sessionNavFrame. All-zero if item doesn't implement the
// interface — shouldn't happen in practice, since only delegation items
// ever feed enterChildSession/cycleChildSession, but keeps those callers
// simple.
func delegationInfo(item chat.ToolMessageItem) (displayName, model, effort string, startTime time.Time, duration time.Duration) {
	if di, ok := item.(chat.DelegationInfoProvider); ok {
		return di.DelegationInfo()
	}
	return "", "", "", time.Time{}, 0
}

// childSessionLevelName formats one breadcrumb level's plain text: the
// delegation's agent name (falling back to the prompt-snippet label if the
// item couldn't be resolved — see delegationInfo), plus a "(n/m)" sibling
// counter when that level had more than one sibling to cycle through.
// "main" is deliberately not part of any level — the back button already
// means "go up", and the levels shown are exactly what alt+up walks back
// through.
func childSessionLevelName(frame sessionNavFrame) string {
	name := frame.agentName
	if name == "" {
		name = frame.label
	}
	if len(frame.siblings) > 1 {
		name = fmt.Sprintf("%s (%d/%d)", name, frame.siblingIndex+1, len(frame.siblings))
	}
	return name
}

// childSessionCurrentActivity builds the current level's compact activity
// parenthetical: the delegation's own prompt snippet (frame.label, already
// first-line-and-~40-chars via childSessionLabel), how many steps it's
// taken so far, and — only while still running — the last tool call. Steps
// and the last tool come from the currently loaded chat directly (m.chat
// holds the child session's own items while it's being viewed, so this
// reflects whichever level is deepest/current regardless of how many
// levels are stacked above it) rather than from stale data captured at
// enterChildSession time. Returns "" if there's nothing to show.
func (m *UI) childSessionCurrentActivity() string {
	var parts []string
	if len(m.navStack) > 0 {
		if label := m.navStack[len(m.navStack)-1].label; label != "" && label != "subagent" {
			parts = append(parts, label)
		}
	}
	if m.chat != nil {
		if n := m.chat.ToolStepCount(); n > 0 {
			parts = append(parts, fmt.Sprintf("step %d", n))
		}
		if m.isAgentBusy() {
			if tc, ok := m.chat.LastToolCall(); ok {
				if summary := chat.LastToolSummary(tc); summary != "" {
					parts = append(parts, "→ "+summary)
				}
			}
		}
	}
	return presentation.JoinStatusParts(parts, -1)
}

// childSessionHeaderText renders row 1's breadcrumb — levels joined by
// " › ", the last one bold with its activity parenthetical — degrading
// through progressively shorter candidates until one fits avail, in the
// agreed sacrifice order: the activity parenthetical goes first, then
// middle levels collapse to a single "…", and only as a last resort is the
// current name itself hard-truncated.
func (m *UI) childSessionHeaderText(s *styles.Styles, avail int) string {
	cb := &s.ChildBanner
	n := len(m.navStack)
	last := m.navStack[n-1]
	lastName := cb.Current.Render(childSessionLevelName(last))

	renderRange := func(lo, hi int) string {
		var b strings.Builder
		for i := lo; i < hi; i++ {
			if i > lo {
				b.WriteString(cb.Sep.Render(" › "))
			}
			b.WriteString(cb.Path.Render(childSessionLevelName(m.navStack[i])))
		}
		return b.String()
	}

	ancestors := renderRange(0, n-1)
	sep := ""
	if ancestors != "" {
		sep = cb.Sep.Render(" › ")
	}

	activity := ""
	if a := m.childSessionCurrentActivity(); a != "" {
		activity = " " + cb.Base.Render("("+a+")")
	}

	// Candidate 1: everything.
	if full := ancestors + sep + lastName + activity; ansi.StringWidth(full) <= avail {
		return full
	}
	// Candidate 2: drop the activity parenthetical.
	if withoutActivity := ancestors + sep + lastName; ansi.StringWidth(withoutActivity) <= avail {
		return withoutActivity
	}
	// Candidate 3: collapse every ancestor between the root and the
	// current level into a single "…" (only meaningful with 2+ ancestors).
	if n > 2 {
		collapsed := cb.Path.Render(childSessionLevelName(m.navStack[0])) +
			cb.Sep.Render(" › … › ") + lastName
		if ansi.StringWidth(collapsed) <= avail {
			return collapsed
		}
	}
	// Candidate 4: the current level alone, no ancestors.
	if ansi.StringWidth(lastName) <= avail {
		return lastName
	}
	// Last resort: hard-truncate the current level's own name.
	return ansi.Truncate(lastName, avail, "…")
}

// drawChildSessionPanel draws the info panel that replaces the editor while
// a sub-agent session is being viewed, in three compact rows:
//
//  1. the breadcrumb of subagent names (never "main" — the back button
//     already means "go up"), each with a "(n/m)" sibling counter when
//     relevant, the last one bold with its live activity in parens —
//     e.g. "developer-junior (3/3) › task (Read the entire file… · step 34)"
//     — and the "back" button, styled as a real accent-filled button
//     rather than a text link. Narrow terminals shed, in order: the
//     activity parenthetical, then middle levels (collapsed to "…"), then
//     truncate the current name itself — see childSessionHeaderText;
//  2. the delegation's model/effort override;
//  3. the child session's own cumulative token usage and either a live
//     ticking elapsed time (still running) or the final duration (done).
//
// A click anywhere in area exits the child session (see the MouseClickMsg
// handling in Update), so hovering anywhere in the panel highlights the
// visible back button.
func (m *UI) drawChildSessionPanel(scr uv.Screen, area uv.Rectangle) {
	m.childPanelButtonRect = uv.Rectangle{}
	if area.Dy() <= 0 || area.Dx() <= 0 || len(m.navStack) == 0 {
		return
	}
	sty := &m.com.Styles.ChildBanner
	width := area.Dx()
	frame := m.navStack[len(m.navStack)-1]

	// Row 1: the level breadcrumb (see childSessionHeaderText for
	// the degrade-under-width-pressure logic) and the back button.
	buttonSty := sty.Button
	if m.childPanelHover {
		buttonSty = sty.ButtonHover
	}
	button := buttonSty.Render(m.childPanelButtonLabel())
	buttonWidth := ansi.StringWidth(button)

	const gap = 1
	row1 := area
	row1.Max.Y = row1.Min.Y + 1
	if avail := width - buttonWidth - gap; avail < 0 {
		// Terminal too narrow for both the breadcrumb and button; drop
		// the breadcrumb entirely.
		uv.NewStyledString(ansi.Truncate(button, width, "")).Draw(scr, row1)
	} else {
		header := m.childSessionHeaderText(m.com.Styles, avail)
		pad := max(0, avail-ansi.StringWidth(header))
		row := header + strings.Repeat(" ", pad+gap) + button
		uv.NewStyledString(row).Draw(scr, row1)

		m.childPanelButtonRect = uv.Rectangle{
			Min: uv.Position{X: area.Max.X - buttonWidth, Y: area.Min.Y},
			Max: area.Max,
		}
		m.childPanelButtonRect.Max.Y = area.Min.Y + 1
	}

	// Row 2: model/effort override — "default model" when the delegation
	// has none (agentic_fetch, or an agent tool inheriting the app's
	// default), so the row is never blank.
	if area.Dy() >= 2 {
		line := childPanelModelSubtitle(frame.model, frame.effort)
		if line == "" {
			line = "default model"
		}
		row2 := area
		row2.Min.Y = area.Min.Y + 1
		row2.Max.Y = row2.Min.Y + 1
		uv.NewStyledString(sty.Base.Render(ansi.Truncate(line, width, "…"))).Draw(scr, row2)
	}

	// Row 3: the child session's own cumulative token usage and context
	// percentage, plus a live elapsed time while still running or the
	// frozen total once done.
	if area.Dy() >= 3 {
		line := childPanelTokensLine(m.session)
		if pct := m.childPanelContextPercent(frame); pct != "" {
			line += " · " + pct
		}
		if e := m.childPanelElapsedText(frame); e != "" {
			line += " · " + e
		}
		row3 := area
		row3.Min.Y = area.Min.Y + 2
		row3.Max.Y = row3.Min.Y + 1
		uv.NewStyledString(sty.Base.Render(ansi.Truncate(line, width, "…"))).Draw(scr, row3)
	}
}

// childPanelModelSubtitle formats the model/effort line for the
// child-session panel, mirroring renderAgentSubtitle's collapsed delegation
// block subtitle in chat/agent.go. "" when the delegation has no override
// to report (agentic_fetch, or an agent tool using the app's default
// model).
func childPanelModelSubtitle(model, effort string) string {
	switch {
	case model != "" && effort != "":
		return model + " · effort " + effort
	case model != "":
		return model
	case effort != "":
		return "effort " + effort
	default:
		return ""
	}
}

// childPanelTokensLine renders the child session's own cumulative token
// usage, split into prompt/completion, e.g. "1.0k tok (800 in / 200 out)".
// session may be nil (not yet loaded).
func childPanelTokensLine(session *session.Session) string {
	if session == nil {
		return "0 tok"
	}
	total := session.PromptTokens + session.CompletionTokens
	if total <= 0 {
		return "0 tok"
	}
	return fmt.Sprintf("%s tok (%s in / %s out)",
		presentation.FormatTokenCount(total),
		presentation.FormatTokenCount(session.PromptTokens),
		presentation.FormatTokenCount(session.CompletionTokens))
}

// childPanelContextPercent reports how full the child session's context
// window is, e.g. "34% ctx" — the same ContextUsed/ContextWindow
// approximation the sidebar's ModelInfo uses for the main session.
// Returns "" when nothing has been used yet or the delegation's model
// (and thus its window size) can't be resolved.
func (m *UI) childPanelContextPercent(frame sessionNavFrame) string {
	if m.session == nil {
		return ""
	}
	used := m.session.PromptTokens + m.session.CompletionTokens
	if used <= 0 {
		return ""
	}
	window := m.childPanelContextWindow(frame)
	if window <= 0 {
		return ""
	}
	return fmt.Sprintf("%d%% ctx", int(float64(used)/float64(window)*100))
}

// childPanelContextWindow resolves the context window of the model the
// delegation runs on: its own "provider/model-id" override when set (the
// provider is the part before the FIRST slash — model ids may contain
// slashes themselves), otherwise the app's main model, which is what a
// delegation without an override inherits.
func (m *UI) childPanelContextWindow(frame sessionNavFrame) int64 {
	if frame.model != "" {
		provider, modelID, ok := strings.Cut(frame.model, "/")
		if !ok {
			return 0
		}
		if mdl := m.com.Config().GetModel(provider, modelID); mdl != nil {
			return mdl.ContextWindow
		}
		return 0
	}
	if sel := m.selectedModel(); sel != nil {
		return sel.CatalogCfg.ContextWindow
	}
	return 0
}

// childPanelElapsedText reports how long the delegation has been running,
// for row 3. A frozen delegationDuration (see sessionNavFrame's doc) wins
// when present — the delegation is done. Otherwise, if the child session
// is still busy, it computes a live elapsed time from delegationStart on
// every draw (there's no dedicated ticker for this — the panel redraws
// often enough while an agent is active that this reads as ticking).
// Returns "" when neither is available, matching renderDelegationOutcomeLine's
// policy of omitting a misleading/unknown duration rather than guessing.
func (m *UI) childPanelElapsedText(frame sessionNavFrame) string {
	if frame.delegationDuration > 0 {
		return presentation.FormatElapsed(frame.delegationDuration)
	}
	if m.isAgentBusy() && !frame.delegationStart.IsZero() {
		return presentation.FormatElapsed(time.Since(frame.delegationStart)) + " elapsed"
	}
	return ""
}
