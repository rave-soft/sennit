package model

import (
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// childBannerButtonLabel is the explicit, clickable "go back up" affordance
// on the right of the child-session banner (see drawChildSessionBanner).
const childBannerButtonLabel = "↑ exit up (alt+up)"

// childSessionBreadcrumbChain returns the ordered list of session labels
// from the root down to the currently viewed child, for
// drawChildSessionBanner: element 0 is the root session's title ("main"),
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

// drawChildSessionBanner draws the persistent, high-contrast "where am I"
// banner at the top of the chat area while a sub-agent session is being
// viewed. It's the main orientation cue while drilled into a child
// session: the full nesting chain on the left ("main › agent1 › agent2",
// current level bolded) and an explicit "exit up" button on the right.
// A click anywhere in area exits the child session (see the
// MouseClickMsg handling in Update); the button itself gets hover
// feedback tracked via m.childBannerButtonRect (set here, read from
// MouseMotionMsg).
func (m *UI) drawChildSessionBanner(scr uv.Screen, area uv.Rectangle) {
	m.childBannerButtonRect = uv.Rectangle{}
	if area.Dy() <= 0 || area.Dx() <= 0 {
		return
	}
	sty := &m.com.Styles.ChildBanner

	buttonSty := sty.Button
	if m.childBannerHover {
		buttonSty = sty.ButtonHover
	}
	button := buttonSty.Render(childBannerButtonLabel)
	buttonWidth := ansi.StringWidth(button)

	const gap = 1
	width := area.Dx()
	avail := width - buttonWidth - gap
	if avail < 0 {
		// Terminal too narrow for both path and button; drop the path.
		uv.NewStyledString(ansi.Truncate(button, width, "")).Draw(scr, area)
		return
	}

	var b strings.Builder
	chain := m.childSessionBreadcrumbChain()
	for i, label := range chain {
		if i > 0 {
			b.WriteString(sty.Sep.Render(" › "))
		}
		if i == len(chain)-1 {
			b.WriteString(sty.Current.Render(label))
		} else {
			b.WriteString(sty.Path.Render(label))
		}
	}
	path := ansi.Truncate(b.String(), avail, "…")
	pad := max(0, avail-ansi.StringWidth(path))

	row := path + sty.Base.Render(strings.Repeat(" ", pad+gap)) + button
	uv.NewStyledString(row).Draw(scr, area)

	m.childBannerButtonRect = uv.Rectangle{
		Min: uv.Position{X: area.Max.X - buttonWidth, Y: area.Min.Y},
		Max: area.Max,
	}
}
