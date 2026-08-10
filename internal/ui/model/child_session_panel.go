package model

import (
	"fmt"
	"strings"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/chat"
)

// childSessionPanelHeight is the fixed height of the child-session info
// panel that replaces the editor while a sub-agent session is being
// viewed (see drawChildSessionPanel): breadcrumb + name + back button,
// model/effort, token usage + elapsed/duration.
const childSessionPanelHeight = 3

// childPanelButtonLabel is the explicit, clickable "go back up" affordance
// on the child-session panel (see drawChildSessionPanel).
const childPanelButtonLabel = "↑ back (alt+up)"

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

// childSessionBreadcrumbChain returns the ordered list of session labels
// from the root down to the currently viewed child, for
// drawChildSessionPanel: element 0 is the root session's title ("main"),
// and each subsequent element is one navStack frame's label (see
// sessionNavFrame.label). The last element is always the level currently
// being viewed.
func (m *UI) childSessionBreadcrumbChain() []string {
	if len(m.navStack) == 0 {
		return nil
	}
	chain := make([]string, 0, len(m.navStack)+1)
	chain = append(chain, m.navStack[0].parentTitle)
	for _, frame := range m.navStack {
		chain = append(chain, frame.label)
	}
	return chain
}

// drawChildSessionPanel draws the info panel that replaces the editor while
// a sub-agent session is being viewed, in three compact rows:
//
//  1. the ancestor breadcrumb (muted) leading into the subagent's own name
//     (bold/accented) with a sibling counter when relevant
//     ("main › agent1 › developer (2/3)"), and the "back" button, styled
//     as a real accent-filled button rather than a text link;
//  2. the delegation's model/effort override;
//  3. the child session's own cumulative token usage and either a live
//     ticking elapsed time (still running) or the final duration (done).
//
// A click anywhere in area exits the child session (see the MouseClickMsg
// handling in Update); the button itself gets hover feedback tracked via
// m.childPanelButtonRect (set here, read from MouseMotionMsg) — this is the
// same click/hover mechanic the old top-of-chat banner used, moved down to
// replace the editor instead of sitting above it.
func (m *UI) drawChildSessionPanel(scr uv.Screen, area uv.Rectangle) {
	m.childPanelButtonRect = uv.Rectangle{}
	if area.Dy() <= 0 || area.Dx() <= 0 || len(m.navStack) == 0 {
		return
	}
	sty := &m.com.Styles.ChildBanner
	width := area.Dx()
	frame := m.navStack[len(m.navStack)-1]

	// Row 1: muted ancestor breadcrumb, the subagent's own name in bold
	// (with a sibling counter when there's more than one to cycle
	// through), and the back button.
	buttonSty := sty.Button
	if m.childPanelHover {
		buttonSty = sty.ButtonHover
	}
	button := buttonSty.Render(childPanelButtonLabel)
	buttonWidth := ansi.StringWidth(button)

	const gap = 1
	row1 := area
	row1.Max.Y = row1.Min.Y + 1
	if avail := width - buttonWidth - gap; avail < 0 {
		// Terminal too narrow for both name and button; drop the name.
		uv.NewStyledString(ansi.Truncate(button, width, "")).Draw(scr, row1)
	} else {
		name := frame.agentName
		if name == "" {
			name = frame.label
		}
		if len(frame.siblings) > 1 {
			name = fmt.Sprintf("%s (%d/%d)", name, frame.siblingIndex+1, len(frame.siblings))
		}

		var b strings.Builder
		if chain := m.childSessionBreadcrumbChain(); len(chain) > 1 {
			b.WriteString(sty.Path.Render(strings.Join(chain[:len(chain)-1], " › ")))
			b.WriteString(sty.Sep.Render(" › "))
		}
		b.WriteString(sty.Current.Render(name))

		path := ansi.Truncate(b.String(), avail, "…")
		pad := max(0, avail-ansi.StringWidth(path))
		row := path + strings.Repeat(" ", pad+gap) + button
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

	// Row 3: the child session's own cumulative token usage, plus a live
	// elapsed time while still running or the frozen total once done.
	if area.Dy() >= 3 {
		line := childPanelTokensLine(m.session)
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
		childPanelTokenCount(total),
		childPanelTokenCount(session.PromptTokens),
		childPanelTokenCount(session.CompletionTokens))
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
		return childPanelFormatElapsed(frame.delegationDuration)
	}
	if m.isAgentBusy() && !frame.delegationStart.IsZero() {
		return childPanelFormatElapsed(time.Since(frame.delegationStart)) + " elapsed"
	}
	return ""
}

// childPanelFormatElapsed renders a duration the way the panel wants it —
// "45s", "4m12s", "1h02m" — mirroring formatElapsed in chat/agent.go.
func childPanelFormatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// childPanelTokenCount renders large token counts compactly ("12.3k"),
// mirroring formatTokenCount in chat/agent.go.
func childPanelTokenCount(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}
