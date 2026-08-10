package model

import (
	"fmt"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/ui/chat"
)

// childSessionPanelHeight is the fixed height of the child-session info
// panel that replaces the editor while a sub-agent session is being
// viewed (see drawChildSessionPanel): breadcrumb + back button, model/
// effort + running/done state, token usage.
const childSessionPanelHeight = 3

// childPanelButtonLabel is the explicit, clickable "go back up" affordance
// on the child-session panel (see drawChildSessionPanel).
const childPanelButtonLabel = "↑ back (alt+up)"

// delegationInfo resolves a delegation tool item's display name and
// model/effort override (see chat.DelegationInfoProvider) for capture into
// a sessionNavFrame. All-empty if item doesn't implement the interface —
// shouldn't happen in practice, since only delegation items ever feed
// enterChildSession/cycleChildSession, but keeps those callers simple.
func delegationInfo(item chat.ToolMessageItem) (displayName, model, effort string) {
	if di, ok := item.(chat.DelegationInfoProvider); ok {
		return di.DelegationInfo()
	}
	return "", "", ""
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
//  1. the full nesting chain with a sibling counter on the current level
//     ("main › agent1 › developer (2/3)") and the "back" button;
//  2. the delegation's model/effort override and its running/done state;
//  3. the child session's own cumulative token usage.
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

	// Row 1: breadcrumb chain (with a sibling counter on the current
	// level) and the back button.
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
		// Terminal too narrow for both path and button; drop the path.
		uv.NewStyledString(ansi.Truncate(button, width, "")).Draw(scr, row1)
	} else {
		var b strings.Builder
		chain := m.childSessionBreadcrumbChain()
		for i, label := range chain {
			if i > 0 {
				b.WriteString(sty.Sep.Render(" › "))
			}
			if i == len(chain)-1 {
				if len(frame.siblings) > 1 {
					label = fmt.Sprintf("%s (%d/%d)", label, frame.siblingIndex+1, len(frame.siblings))
				}
				b.WriteString(sty.Current.Render(label))
			} else {
				b.WriteString(sty.Path.Render(label))
			}
		}
		path := ansi.Truncate(b.String(), avail, "…")
		pad := max(0, avail-ansi.StringWidth(path))
		row := path + sty.Base.Render(strings.Repeat(" ", pad+gap)) + button
		uv.NewStyledString(row).Draw(scr, row1)

		m.childPanelButtonRect = uv.Rectangle{
			Min: uv.Position{X: area.Max.X - buttonWidth, Y: area.Min.Y},
			Max: area.Max,
		}
		m.childPanelButtonRect.Max.Y = area.Min.Y + 1
	}

	noteSty := m.com.Styles.Tool.TodoStatusNote

	// Row 2: model/effort override and running/done state.
	if area.Dy() >= 2 {
		state := "done"
		if m.isAgentBusy() {
			state = "running"
		}
		line := state
		if sub := childPanelModelSubtitle(frame.model, frame.effort); sub != "" {
			line += " · " + sub
		}
		row2 := area
		row2.Min.Y = area.Min.Y + 1
		row2.Max.Y = row2.Min.Y + 1
		uv.NewStyledString(noteSty.Render(ansi.Truncate(line, width, "…"))).Draw(scr, row2)
	}

	// Row 3: the child session's own cumulative token usage.
	if area.Dy() >= 3 {
		line := "0 tok"
		if m.session != nil {
			if total := m.session.PromptTokens + m.session.CompletionTokens; total > 0 {
				line = fmt.Sprintf("%s tok (%s prompt / %s completion)",
					childPanelTokenCount(total),
					childPanelTokenCount(m.session.PromptTokens),
					childPanelTokenCount(m.session.CompletionTokens))
			}
		}
		row3 := area
		row3.Min.Y = area.Min.Y + 2
		row3.Max.Y = row3.Min.Y + 1
		uv.NewStyledString(noteSty.Render(ansi.Truncate(line, width, "…"))).Draw(scr, row3)
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

// childPanelTokenCount renders large token counts compactly ("12.3k"),
// mirroring formatTokenCount in chat/agent.go.
func childPanelTokenCount(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}
