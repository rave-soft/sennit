package model

// The threads dashboard's administration chrome: the toolbar of buttons,
// the status filter tabs, and the detail pane under the list. threads.go
// owns the screen's state, layout, and list; this file owns the pieces
// that are pointed at with a mouse — what they contain, when they are
// enabled, and where they land on screen.
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
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// threadsFilter is the status class the list is narrowed to. The tabs are
// coarse on purpose: an operator asks "what is running", "what needs
// attention", not "show me exactly the merging ones".
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

// matches reports whether a thread belongs in this tab. Statuses not named
// by any narrower tab (merging, cancelled, interrupted, ...) still appear
// under All, so no delegation can hide from the screen entirely.
func (f threadsFilter) matches(t proto.Thread) bool {
	status := proto.ThreadStatus(t.Status)
	switch f {
	case filterRunning:
		return status == proto.ThreadStatusRunning || status == proto.ThreadStatusMerging
	case filterIdle:
		return status == proto.ThreadStatusIdle
	case filterDone:
		return status == proto.ThreadStatusCompleted || status == proto.ThreadStatusMerged
	case filterFailed:
		return status == proto.ThreadStatusFailed ||
			status == proto.ThreadStatusConflict ||
			status == proto.ThreadStatusMergeBlocked
	default:
		return true
	}
}

// filterThreads returns the threads f admits, preserving order.
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
	actionNew threadAction = iota
	actionOpen
	actionMerge
	actionCancel
	actionRemove
	actionRefresh
	actionBack
)

// threadsToolbarActions is the button order, left to right. Destructive
// actions sit at the end, away from Open, so a mis-click on the busiest
// button is not the one that tears a worktree down.
var threadsToolbarActions = []threadAction{
	actionNew, actionOpen, actionMerge, actionCancel, actionRemove, actionRefresh,
}

// label is the button's text. The key hint rides along in the footer help
// line rather than inside the button, which keeps the toolbar scannable at
// narrow widths.
func (a threadAction) label() string {
	switch a {
	case actionNew:
		return "+ New"
	case actionOpen:
		return "Open"
	case actionMerge:
		return "Merge"
	case actionCancel:
		return "Cancel"
	case actionRemove:
		return "Remove"
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
	return a == actionRemove
}

// enabledFor reports whether the action can run against the given
// selection (nil when the list is empty or nothing is selected). The
// answer drives both the button's appearance and the key binding, so a
// dimmed button and a dead shortcut can never disagree.
func (a threadAction) enabledFor(sel *proto.Thread) bool {
	switch a {
	case actionNew, actionRefresh, actionBack:
		return true
	case actionOpen, actionRemove:
		return sel != nil
	case actionMerge:
		return sel != nil && threadMergeable(sel.Kind, sel.Status)
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
	case proto.ThreadStatusRunning, proto.ThreadStatusMerging:
		return sty.Threads.StatusRunning
	case proto.ThreadStatusCompleted, proto.ThreadStatusMerged:
		return sty.Threads.StatusDone
	case proto.ThreadStatusFailed, proto.ThreadStatusConflict, proto.ThreadStatusMergeBlocked:
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
	name    int
	status  int
	branch  int // 0 when dropped
	updated int // 0 when dropped
	goal    int // 0 when there is no room left
}

// computeThreadsColumns lays the table out for a given total width.
func computeThreadsColumns(width int) threadsColumns {
	const (
		gap        = 2
		nameWidth  = 22
		statusW    = 10
		branchW    = 26
		updatedW   = 14
		minGoal    = 12
		minNameCol = 12
	)

	c := threadsColumns{name: nameWidth, status: statusW, branch: branchW, updated: updatedW}
	fits := func(cc threadsColumns) int {
		total := cc.name + gap + cc.status
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
	b.WriteString(padTo("NAME", c.name))
	b.WriteString("  " + padTo("STATUS", c.status))
	if c.branch > 0 {
		b.WriteString("  " + padTo("BRANCH", c.branch))
	}
	if c.updated > 0 {
		b.WriteString("  " + padTo("UPDATED", c.updated))
	}
	if c.goal > 0 {
		b.WriteString("  " + padTo("GOAL", c.goal))
	}
	return sty.Threads.ColumnHeader.Render(ansi.Truncate(b.String(), width, "…"))
}

// padTo pads (or truncates) s to exactly n cells.
func padTo(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = ansi.Truncate(s, n, "…")
	if pad := n - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// threadsDetailLines renders the detail pane's content for the selected
// thread: the fields too long for a table row (the full goal, the branch
// pair, timings, and whatever the run left behind — a result summary or an
// error). Returns nil when nothing is selected.
func threadsDetailLines(sty *styles.Styles, sel *proto.Thread, width int) []string {
	if sel == nil {
		return nil
	}

	field := func(label, value string) string {
		if value == "" {
			return ""
		}
		l := sty.Threads.DetailLabel.Render(padTo(label, 10))
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

	// The outcome line is the reason this pane exists: a failed thread's
	// error is the one thing a table row can never show, and it is exactly
	// what someone opening this screen is looking for.
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
