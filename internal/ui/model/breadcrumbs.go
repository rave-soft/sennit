package model

import (
	"image"
	"strings"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/ui/util"
)

// The breadcrumb bar occupies the full-width horizontal rule between the
// chat and the editor (see drawChatSeparators). It answers "where am I, and
// how do I get out" in the one place that is on screen in every mode:
// drilling into a thread and drilling into a sub-agent are the same
// navigation to the user, so they share one trail rather than each growing
// its own affordance.
//
// The rule stays a plain rule at the top level, where there is nowhere to
// go back to. Height never changes either way, so nothing below it moves.

// breadcrumbRootName is the first crumb of every non-empty trail: the
// top-level session the whole path hangs off. It is only rendered when
// there is at least one level below it — a lone "main" would be noise.
const breadcrumbRootName = "main"

// leaveThreadRequestedMsg asks the router to leave the attached thread.
// A thread's embedded UI cannot tear down its own attachment (Root owns the
// workspace and the event pump), so the Back button raises this instead and
// root.go turns it into leaveThreadToMain.
type leaveThreadRequestedMsg struct{}

// breadcrumbCrumbs builds the trail from the outside in: the root session,
// then the thread this UI is embedded in (if any — see UI.crumbRoot), then
// one level per sub-agent session that has been drilled into. Returns nil
// at the top level, which is what keeps the rule plain there.
func (m *UI) breadcrumbCrumbs() []string {
	if m.crumbRoot == "" && len(m.sess.navStack) == 0 {
		return nil
	}
	crumbs := []string{breadcrumbRootName}
	if m.crumbRoot != "" {
		crumbs = append(crumbs, m.crumbRoot)
	}
	for _, frame := range m.sess.navStack {
		crumbs = append(crumbs, childSessionLevelName(frame))
	}
	return crumbs
}

// breadcrumbButtonLabel is the explicit, clickable "go up one level"
// affordance at the end of the bar. Both levels it can leave — a sub-agent
// session and a thread — are bound to the same key, so one label covers
// both.
func (m *UI) breadcrumbButtonLabel() string {
	return "↑ Back (" + m.exitChildSessionShortcut() + ")"
}

// breadcrumbBack performs what the Back button promises, one level at a
// time: out of the deepest sub-agent session first, and only once none are
// left, out of the thread itself. Nil when there is nowhere to go.
func (m *UI) breadcrumbBack() tea.Cmd {
	if m.viewingChildSession() {
		return m.exitChildSession()
	}
	if m.embedded {
		return util.CmdHandler(leaveThreadRequestedMsg{})
	}
	return nil
}

// breadcrumbBarPlan is the resolved geometry of one bar: the rendered
// trail, the rendered button, and how much rule separates them.
type breadcrumbBarPlan struct {
	trail      string
	button     string
	buttonRect image.Rectangle
	fill       int
	// trailWidth is the display width of trail, kept alongside it because
	// trail carries styling and cannot be measured for free.
	trailWidth int
}

// Bar geometry, laid out as:
//
//	─ main › thread › agent ──────── [ ↑ Back (alt+up) ] ─
//	^ ^                    ^        ^                    ^
//	| padding              padding  fill          padding|
//	lead                                              tail
//
// The lead and tail keep the bar reading as a rule rather than a row of
// text, and the fill has to stay at least one cell wide for the same
// reason.
const (
	breadcrumbLead    = 1
	breadcrumbTail    = 1
	breadcrumbPadding = 1
	breadcrumbMinFill = 1
	// breadcrumbMinTrail is the narrowest trail worth keeping. Below it the
	// trail is dropped entirely and the button keeps the bar, since a
	// two-character stub of a name tells the user nothing while the way out
	// still matters.
	breadcrumbMinTrail = 8
)

// planBreadcrumbBar resolves the bar for area, or reports ok=false when
// there is nothing to draw (top level) or no room to draw it (the caller
// then falls back to a plain rule). It is a pure function of UI state and
// the area, so the mouse handlers can recompute the button's hit box
// without depending on Draw having painted the current layout first — the
// same reason sessionPanelRowLayout is recomputed rather than cached.
func (m *UI) planBreadcrumbBar(area uv.Rectangle) (breadcrumbBarPlan, bool) {
	crumbs := m.breadcrumbCrumbs()
	if len(crumbs) == 0 || area.Dx() <= 0 {
		return breadcrumbBarPlan{}, false
	}

	sty := m.com.Styles.ChildBanner
	buttonSty := sty.Button
	if m.breadcrumbHover {
		buttonSty = sty.ButtonHover
	}
	button := buttonSty.Render(m.breadcrumbButtonLabel())
	buttonWidth := ansi.StringWidth(button)

	width := area.Dx()
	// Everything in the bar that is neither the trail nor the fill: the
	// lead and tail rule stubs, and one pad on each side of both the trail
	// and the button.
	const overhead = breadcrumbLead + breadcrumbTail + 4*breadcrumbPadding
	avail := width - overhead - buttonWidth - breadcrumbMinFill
	if avail < breadcrumbMinTrail {
		// No room for a meaningful trail; keep the button alone, right
		// aligned, if even that fits. Its own row is
		// fill + pad + button + pad + tail.
		const buttonOnlyOverhead = breadcrumbTail + 2*breadcrumbPadding
		if width < buttonWidth+buttonOnlyOverhead+breadcrumbMinFill {
			return breadcrumbBarPlan{}, false
		}
		fill := width - buttonWidth - buttonOnlyOverhead
		return breadcrumbBarPlan{
			button: button,
			fill:   fill,
			buttonRect: image.Rect(
				area.Min.X+fill+breadcrumbPadding, area.Min.Y,
				area.Min.X+fill+breadcrumbPadding+buttonWidth, area.Min.Y+1,
			),
		}, true
	}

	trail := m.breadcrumbText(crumbs, avail)
	trailWidth := ansi.StringWidth(trail)
	fill := width - overhead - buttonWidth - trailWidth
	buttonX := area.Min.X + breadcrumbLead + breadcrumbPadding + trailWidth + breadcrumbPadding + fill + breadcrumbPadding
	return breadcrumbBarPlan{
		trail:      trail,
		trailWidth: trailWidth,
		button:     button,
		fill:       fill,
		buttonRect: image.Rect(buttonX, area.Min.Y, buttonX+buttonWidth, area.Min.Y+1),
	}, true
}

// breadcrumbText renders the trail — crumbs joined by " › ", the last one
// emphasized and carrying the current level's live activity — degrading
// through progressively shorter candidates until one fits avail. The
// sacrifice order is deliberate: the activity parenthetical is the most
// disposable, then the middle of the path (its ends say the most about
// where you are), and only as a last resort is the current name itself
// truncated, since that is the one crumb the user is actually looking at.
func (m *UI) breadcrumbText(crumbs []string, avail int) string {
	cb := &m.com.Styles.ChildBanner
	n := len(crumbs)
	lastName := cb.Current.Render(crumbs[n-1])

	renderRange := func(lo, hi int) string {
		var b strings.Builder
		for i := lo; i < hi; i++ {
			if i > lo {
				b.WriteString(cb.Sep.Render(" › "))
			}
			b.WriteString(cb.Path.Render(crumbs[i]))
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
	// Candidate 3: collapse everything between the root and the current
	// level into a single "…" (only meaningful with 2+ ancestors).
	if n > 2 {
		collapsed := cb.Path.Render(crumbs[0]) + cb.Sep.Render(" › … › ") + lastName
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

// drawBreadcrumbBar paints the navigation bar over the rule row and reports
// whether it did. False means the caller should draw its plain rule
// instead: either there is nowhere to go back to, or the terminal is too
// narrow to say anything useful.
func (m *UI) drawBreadcrumbBar(scr uv.Screen, area uv.Rectangle) bool {
	m.breadcrumbButtonRect = image.Rectangle{}
	plan, ok := m.planBreadcrumbBar(area)
	if !ok {
		return false
	}

	rule := m.com.Styles.Messages.ChatSeparator
	pad := strings.Repeat(" ", breadcrumbPadding)

	var b strings.Builder
	if plan.trail != "" {
		b.WriteString(rule.Render(strings.Repeat(styles.SectionSeparator, breadcrumbLead)))
		b.WriteString(pad)
		b.WriteString(plan.trail)
		b.WriteString(pad)
	}
	b.WriteString(rule.Render(strings.Repeat(styles.SectionSeparator, plan.fill)))
	b.WriteString(pad)
	b.WriteString(plan.button)
	b.WriteString(pad)
	b.WriteString(rule.Render(strings.Repeat(styles.SectionSeparator, breadcrumbTail)))

	uv.NewStyledString(b.String()).Draw(scr, area)
	m.breadcrumbButtonRect = plan.buttonRect
	return true
}

// breadcrumbBarRow is the rule row the bar lives on: the one directly above
// the editor. Zero-height when there is no editor area to sit above.
func (m *UI) breadcrumbBarRow() image.Rectangle {
	e := m.lay.layout.editor
	if e.Dx() <= 0 {
		return image.Rectangle{}
	}
	return image.Rect(e.Min.X, e.Min.Y-1, e.Max.X, e.Min.Y)
}

// breadcrumbButtonHit reports whether pt falls on the Back button,
// recomputing the hit box from the current layout rather than trusting the
// rect last painted (see planBreadcrumbBar).
func (m *UI) breadcrumbButtonHit(pt image.Point) bool {
	row := m.breadcrumbBarRow()
	if row.Empty() {
		return false
	}
	plan, ok := m.planBreadcrumbBar(row)
	if !ok {
		return false
	}
	return pt.In(plan.buttonRect)
}
