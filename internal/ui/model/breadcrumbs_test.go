package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// drawSeparatorRow paints the rule row above the editor — the row the
// breadcrumb bar takes over — and returns it as plain text.
func drawSeparatorRow(t *testing.T, u *UI) string {
	t.Helper()
	scr := uv.NewScreenBuffer(u.lay.width, u.lay.height)
	u.drawChatSeparators(scr, u.lay.layout.editor)
	row := u.breadcrumbBarRow()
	var b []rune
	for x := row.Min.X; x < row.Max.X; x++ {
		cell := scr.CellAt(x, row.Min.Y)
		if cell == nil {
			continue
		}
		b = append(b, []rune(cell.Content)...)
	}
	return string(b)
}

// TestBreadcrumbBar_TopLevelStaysAPlainRule: with nowhere to go back to
// there is nothing to say, so the row keeps being the rule it has always
// been. This is what lets the bar cost no height — it is not a new row, it
// is the existing one saying more when there is more to say.
func TestBreadcrumbBar_TopLevelStaysAPlainRule(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.sess.navStack = nil
	u.updateLayoutAndSize()

	row := drawSeparatorRow(t, u)
	require.NotEmpty(t, row)
	for _, r := range row {
		require.Equal(t, styles.SectionSeparator, string(r),
			"the top level must leave the rule untouched")
	}
	require.Nil(t, u.breadcrumbCrumbs())
}

// TestBreadcrumbBar_ShowsPathAndBackButton: drilled into sub-agents from
// the main session, the bar names every level from the root down and ends
// with the way out.
func TestBreadcrumbBar_ShowsPathAndBackButton(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)

	row := drawSeparatorRow(t, u)
	require.Contains(t, row, "main", "the trail starts at the top-level session")
	require.Contains(t, row, "developer-junior (3/3)", "ancestor level: name + sibling counter")
	require.Contains(t, row, "› task", "current level separated from its ancestor")
	require.Contains(t, row, "Back", "the bar ends with the way out")
	require.Contains(t, row, styles.SectionSeparator, "it still reads as a rule")
}

// TestBreadcrumbBar_ThreadIsALevelOfItsOwn: a thread is a place you
// navigated into, exactly like a sub-agent, so it takes a crumb of its own
// between the root and whatever was opened inside it.
func TestBreadcrumbBar_ThreadIsALevelOfItsOwn(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.embedded = true
	u.crumbRoot = "fix-hang"

	require.Equal(t,
		[]string{"main", "fix-hang", "developer-junior (3/3)", "task"},
		u.breadcrumbCrumbs())

	// And at the top of that thread, with no sub-agent open, the trail is
	// still there — the thread itself is somewhere you can leave.
	u.sess.navStack = nil
	require.Equal(t, []string{"main", "fix-hang"}, u.breadcrumbCrumbs())
}

// TestBreadcrumbBack_UnwindsOneLevelAtATime: Back means "up one", not "all
// the way out". From inside a sub-agent of a thread it pops the sub-agent
// and leaves the thread attached; only once the sub-agents are gone does it
// ask the router to leave the thread.
func TestBreadcrumbBack_UnwindsOneLevelAtATime(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.embedded = true
	u.crumbRoot = "fix-hang"

	require.NotNil(t, u.breadcrumbBack())
	require.Len(t, u.sess.navStack, 1, "one sub-agent level popped, the thread untouched")

	require.NotNil(t, u.breadcrumbBack())
	require.Empty(t, u.sess.navStack)

	cmd := u.breadcrumbBack()
	require.NotNil(t, cmd, "at the top of a thread, Back must leave the thread")
	require.IsType(t, leaveThreadRequestedMsg{}, cmd(),
		"the embedded UI cannot detach itself; it must ask the router")
}

// TestBreadcrumbBack_TopLevelHasNowhereToGo: on the main session with no
// navigation below it, Back is not offered and does nothing if invoked.
func TestBreadcrumbBack_TopLevelHasNowhereToGo(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.sess.navStack = nil

	require.Nil(t, u.breadcrumbBack())
}

// TestBreadcrumbBar_ClickOnBackGoesUp: the button is a real click target,
// hit-tested from the current layout rather than from whatever a previous
// Draw happened to paint — a click can reach Update before View has painted
// the layout it belongs to.
func TestBreadcrumbBar_ClickOnBackGoesUp(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	plan, ok := u.planBreadcrumbBar(u.breadcrumbBarRow())
	require.True(t, ok)

	_, cmd := u.Update(tea.MouseClickMsg(tea.Mouse{
		X:      plan.buttonRect.Min.X,
		Y:      plan.buttonRect.Min.Y,
		Button: uv.MouseLeft,
	}))

	require.Len(t, u.sess.navStack, 1, "clicking Back must pop exactly one level")
	require.NotNil(t, cmd, "must return the loadSession cmd for the level above")
}

// TestBreadcrumbBar_ClickBesideTheButtonIsNotBack: the rest of the rule is
// not a click target. The trail names levels the user did not ask to leave,
// so only the button acts.
func TestBreadcrumbBar_ClickBesideTheButtonIsNotBack(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	row := u.breadcrumbBarRow()

	u.Update(tea.MouseClickMsg(tea.Mouse{X: row.Min.X, Y: row.Min.Y, Button: uv.MouseLeft}))
	require.Len(t, u.sess.navStack, 2, "clicking the trail must not navigate")
}

// TestBreadcrumbBar_ButtonHover: pointing at the button highlights it,
// moving off clears it.
func TestBreadcrumbBar_ButtonHover(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	plan, ok := u.planBreadcrumbBar(u.breadcrumbBarRow())
	require.True(t, ok)

	u.Update(tea.MouseMotionMsg(tea.Mouse{X: plan.buttonRect.Min.X, Y: plan.buttonRect.Min.Y}))
	require.True(t, u.breadcrumbHover)

	u.Update(tea.MouseMotionMsg(tea.Mouse{X: 0, Y: 0}))
	require.False(t, u.breadcrumbHover)
}

// TestBreadcrumbBar_KeepsTheWayOutWhenTooNarrowForTheTrail: on a terminal
// too cramped to say anything useful about where you are, the button
// survives and the trail is what goes — a truncated stub of a name helps
// nobody, but being unable to leave is a trap.
func TestBreadcrumbBar_KeepsTheWayOutWhenTooNarrowForTheTrail(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.lay.width = ansi.StringWidth(u.breadcrumbButtonLabel()) + 8
	u.updateLayoutAndSize()

	plan, ok := u.planBreadcrumbBar(u.breadcrumbBarRow())
	require.True(t, ok, "the way out must survive a narrow terminal")
	require.Empty(t, plan.trail)
	require.NotZero(t, plan.buttonRect)
}

// TestBreadcrumbBar_FallsBackToAPlainRuleWhenEvenTheButtonCannotFit: at
// that point there is nothing honest to draw, so the row goes back to being
// a rule rather than showing a mangled button.
func TestBreadcrumbBar_FallsBackToAPlainRuleWhenEvenTheButtonCannotFit(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	row := u.breadcrumbBarRow()
	row.Max.X = row.Min.X + 4

	_, ok := u.planBreadcrumbBar(row)
	require.False(t, ok)

	scr := uv.NewScreenBuffer(u.lay.width, u.lay.height)
	require.False(t, u.drawBreadcrumbBar(scr, row))
}

// TestBreadcrumbText_DropsActivityBeforeAncestors covers the sacrifice
// order under width pressure: the activity parenthetical is the most
// disposable, and the full path is kept as long as it fits.
func TestBreadcrumbText_DropsActivityBeforeAncestors(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.chat.AppendMessages(
		chat.NewToolMessageItem(u.com.Styles, "m1",
			message.ToolCall{ID: "tc-1", Name: "grep", Input: `{"pattern":"login"}`, Finished: true}, nil, false, nil),
	)
	crumbs := u.breadcrumbCrumbs()

	full := u.breadcrumbText(crumbs, 200)
	require.Contains(t, ansi.Strip(full), "Read the entire file", "plenty of width: activity shown")

	narrower := u.breadcrumbText(crumbs, ansi.StringWidth(ansi.Strip(full))-5)
	out := ansi.Strip(narrower)
	require.NotContains(t, out, "Read the entire file", "activity must be dropped first")
	require.Contains(t, out, "developer-junior (3/3)", "the path must still be shown")
	require.Contains(t, out, "task")
}

// TestBreadcrumbText_CollapsesMiddleLevels: once even the plain path
// doesn't fit, the middle collapses to a single "…", keeping the ends —
// where you started and where you are say the most.
func TestBreadcrumbText_CollapsesMiddleLevels(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.sess.navStack[len(u.sess.navStack)-1].label = "" // no activity, to isolate the path
	crumbs := u.breadcrumbCrumbs()

	full := crumbs[0] + " › " + crumbs[1] + " › " + crumbs[2]
	collapsed := crumbs[0] + " › … › " + crumbs[2]
	avail := ansi.StringWidth(collapsed) + 2
	require.Less(t, avail, ansi.StringWidth(full), "test width must be too narrow for the uncollapsed path")

	out := ansi.Strip(u.breadcrumbText(crumbs, avail))
	require.Contains(t, out, "main")
	require.Contains(t, out, "…", "the middle must collapse to an ellipsis")
	require.Contains(t, out, "task")
	require.NotContains(t, out, "developer-junior", "the collapsed level's real name must not appear")
}

// TestBreadcrumbText_DropsAncestorsKeepsCurrentName: too narrow even for
// the collapsed path, so only the level you are on remains.
func TestBreadcrumbText_DropsAncestorsKeepsCurrentName(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.sess.navStack[len(u.sess.navStack)-1].label = ""

	out := ansi.Strip(u.breadcrumbText(u.breadcrumbCrumbs(), len("task")+1))
	require.Equal(t, "task", out)
}

// TestBreadcrumbText_HardTruncatesCurrentName is the last resort: even the
// bare current name doesn't fit, so it is truncated rather than allowed to
// overflow into the rest of the bar.
func TestBreadcrumbText_HardTruncatesCurrentName(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.sess.navStack[len(u.sess.navStack)-1].agentName = "a-very-long-subagent-name"
	u.sess.navStack[len(u.sess.navStack)-1].label = ""

	out := ansi.Strip(u.breadcrumbText(u.breadcrumbCrumbs(), 6))
	require.LessOrEqual(t, ansi.StringWidth(out), 6)
	require.Contains(t, out, "…")
}
