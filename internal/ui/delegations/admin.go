package delegations

// The delegations dashboard's administration chrome: the toolbar of buttons,
// status filter tabs, and detail pane under the list. threads.go owns the
// screen's state, layout, and list; this file owns the pieces that are pointed
// at with a mouse — what they contain, when they are enabled, and where they
// land on screen.
//
// Every button is also a key binding (threadsKeyMap), and both paths
// produce the same message: the toolbar is a second way to reach the
// actions, never a separate code path that can drift from the shortcuts.

import (
	"fmt"
	"image"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/dustin/go-humanize"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/presentation"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// threadsFilter is the status class the list is narrowed to. The tabs are
// coarse on purpose: an operator asks "what is running" or "what needs
// attention", not for every persisted status separately.
type threadsFilter int

const (
	filterAll threadsFilter = iota
	filterRunning
	filterIdle
	filterDone
	filterFailed
)

// threadsFilters is the tab order, left to right.
var threadsFilters = []threadsFilter{filterAll, filterRunning, filterIdle, filterDone, filterFailed}

// label is the tab's text, without its count.
func (f threadsFilter) label() string {
	switch f {
	case filterRunning:
		return "Running"
	case filterIdle:
		return "Idle"
	case filterDone:
		return "Done"
	case filterFailed:
		return "Failed"
	default:
		return "All"
	}
}

// matches reports whether a delegation belongs in this tab. Statuses not
// named by a narrower tab still appear under All, so no delegation can hide
// from the screen entirely.
func (f threadsFilter) matches(t proto.Thread) bool {
	status := proto.ThreadStatus(t.Status)
	switch f {
	case filterRunning:
		return status == proto.ThreadStatusRunning
	case filterIdle:
		return status == proto.ThreadStatusIdle
	case filterDone:
		return status == proto.ThreadStatusCompleted
	case filterFailed:
		return status == proto.ThreadStatusFailed
	default:
		return true
	}
}

// filterThreads returns the delegations f admits, preserving order.
func filterThreads(threads []proto.Thread, f threadsFilter) []proto.Thread {
	if f == filterAll {
		return threads
	}
	out := make([]proto.Thread, 0, len(threads))
	for _, t := range threads {
		if f.matches(t) {
			out = append(out, t)
		}
	}
	return out
}

// threadAction is one toolbar button / key binding.
type threadAction int

const (
	actionOpen threadAction = iota
	actionCancel
	actionCleanup
	actionRefresh
	actionBack
)

// threadsToolbarActions is the button order, left to right. Destructive
// actions sit at the end, away from Open, so a mis-click on the busiest
// button is not the one that tears a worktree down.
var threadsToolbarActions = []threadAction{
	actionOpen, actionCancel, actionCleanup, actionRefresh,
}

// label is the button's text. The key hint rides along in the footer help
// line rather than inside the button, which keeps the toolbar scannable at
// narrow widths.
func (a threadAction) label() string {
	switch a {
	case actionOpen:
		return "Open"
	case actionCancel:
		return "Cancel"
	case actionCleanup:
		return "Cleanup"
	case actionRefresh:
		return "Refresh"
	case actionBack:
		return "← Back"
	default:
		return ""
	}
}

// destructive reports whether the action tears something down, and so is
// rendered in the danger fill when hovered.
func (a threadAction) destructive() bool {
	return a == actionCleanup
}

// enabledFor reports whether the action can run against the given
// selection (nil when the list is empty or nothing is selected). The
// answer drives both the button's appearance and the key binding, so a
// dimmed button and a dead shortcut can never disagree.
func (a threadAction) enabledFor(sel *proto.Thread) bool {
	switch a {
	case actionRefresh, actionBack:
		return true
	case actionOpen, actionCleanup:
		return sel != nil && (proto.ThreadKind(sel.Kind) == proto.ThreadKindThread || sel.Kind == "")
	case actionCancel:
		return sel != nil && !proto.ThreadStatus(sel.Status).Terminal()
	default:
		return false
	}
}

// threadsHitZone is one clickable rectangle and what it stands for.
// Recomputed on every Draw, since the whole layout depends on the current
// size, filter, and selection.
type threadsHitZone struct {
	rect   image.Rectangle
	action threadAction
	filter threadsFilter
	// isFilter distinguishes a tab from a button; action is meaningless
	// on a tab and vice versa.
	isFilter bool
	enabled  bool
}

// threadStatusStyle maps a status onto its dashboard color class.
func threadStatusStyle(sty *styles.Styles, status string) lipgloss.Style {
	switch proto.ThreadStatus(status) {
	case proto.ThreadStatusRunning:
		return sty.Threads.StatusRunning
	case proto.ThreadStatusCompleted:
		return sty.Threads.StatusDone
	case proto.ThreadStatusFailed:
		return sty.Threads.StatusError
	case proto.ThreadStatusCancelled, proto.ThreadStatusInterrupted:
		return sty.Threads.StatusWarn
	default:
		// Idle and anything the server adds later: neutral. Idle is a live
		// delegation with no run in flight, which must not read as done.
		return sty.Threads.StatusIdle
	}
}

// threadsColumns is the width, in cells, of each fixed-form column of the
// list. The goal column takes whatever is left. Columns are dropped from
// the right as the terminal narrows so the name and status — the two
// fields an operator scans by — always survive.
type threadsColumns struct {
	name     int
	status   int
	isolated int
	branch   int // 0 when dropped
	updated  int // 0 when dropped
	goal     int // 0 when there is no room left
}

// computeThreadsColumns lays the table out for a given total width.
func computeThreadsColumns(width int) threadsColumns {
	const (
		gap        = 2
		nameWidth  = 22
		statusW    = 10
		isolatedW  = 8
		branchW    = 26
		updatedW   = 14
		minGoal    = 12
		minNameCol = 12
	)

	c := threadsColumns{name: nameWidth, status: statusW, isolated: isolatedW, branch: branchW, updated: updatedW}
	fits := func(cc threadsColumns) int {
		total := cc.name + gap + cc.status + gap + cc.isolated
		if cc.branch > 0 {
			total += gap + cc.branch
		}
		if cc.updated > 0 {
			total += gap + cc.updated
		}
		return total
	}

	if fits(c)+gap+minGoal > width {
		c.branch = 0
	}
	if fits(c)+gap+minGoal > width {
		c.updated = 0
	}
	if fits(c) > width {
		c.name = max(minNameCol, width-gap-c.status)
	}
	if rest := width - fits(c) - gap; rest >= minGoal {
		c.goal = rest
	}
	return c
}

// renderThreadsColumnHeader renders the table's header row.
func renderThreadsColumnHeader(sty *styles.Styles, c threadsColumns, width int) string {
	var b strings.Builder
	b.WriteString(presentation.PadTo("NAME", c.name))
	b.WriteString("  " + presentation.PadTo("STATUS", c.status))
	b.WriteString("  " + presentation.PadTo("ISOLATED", c.isolated))
	if c.branch > 0 {
		b.WriteString("  " + presentation.PadTo("BRANCH", c.branch))
	}
	if c.updated > 0 {
		b.WriteString("  " + presentation.PadTo("UPDATED", c.updated))
	}
	if c.goal > 0 {
		b.WriteString("  " + presentation.PadTo("GOAL", c.goal))
	}
	return sty.Threads.ColumnHeader.Render(ansi.Truncate(b.String(), width, "…"))
}

// threadsDetailLines renders the detail pane for the selected delegation:
// fields too long for a table row, timings, and the result summary or error.
// Isolated delegations additionally carry branch details. It returns nil when
// nothing is selected.
func threadsDetailLines(sty *styles.Styles, sel *proto.Thread, width int) []string {
	if sel == nil {
		return nil
	}

	field := func(label, value string) string {
		if value == "" {
			return ""
		}
		l := sty.Threads.DetailLabel.Render(presentation.PadTo(label, 10))
		v := sty.Threads.DetailValue.Render(ansi.Truncate(value, max(0, width-11), "…"))
		return l + " " + v
	}

	branches := sel.Branch
	if sel.BaseBranch != "" {
		if branches == "" {
			branches = "→ " + sel.BaseBranch
		} else {
			branches += "  →  " + sel.BaseBranch
		}
	}

	timing := fmt.Sprintf("created %s", humanize.Time(time.Unix(sel.CreatedAt, 0)))
	if sel.CompletedAt > 0 {
		timing += fmt.Sprintf(", finished %s", humanize.Time(time.Unix(sel.CompletedAt, 0)))
	} else {
		timing += fmt.Sprintf(", updated %s", humanize.Time(time.Unix(sel.UpdatedAt, 0)))
	}

	// The outcome line exposes the result or error that cannot fit in a table
	// row.
	outcome := ""
	outcomeLabel := "result"
	if sel.Error != "" {
		outcome = sel.Error
		outcomeLabel = "error"
	} else if sel.ResultSummary != "" {
		outcome = sel.ResultSummary
	}

	lines := []string{
		field("goal", sel.Goal),
		field("branch", branches),
		field("timing", timing),
		field(outcomeLabel, outcome),
	}
	out := lines[:0]
	for _, l := range lines {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
