package chat

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/message"
)

// A delegation's report is the one user-role message nobody wrote. It is
// the answer a background task or thread sent back to the session that
// started it, persisted into that session's history so the reader — the
// model on its next turn, and the person in the transcript — still has
// it a step later (see runTurn.foldCompletions in internal/agent).
//
// Rendered whole it takes over the window. A report is as long as the
// work it describes: a delegated implementation answers with the files
// it touched, the checks it ran and what it left undone, which is
// exactly right for the model and exactly too much for someone scanning
// their own conversation. So it collapses to a row, like every other
// long block in the chat, and opens through the same [Expandable] path.
//
// What the collapsed row must still carry is which delegation reported
// and how it went: a report the reader cannot identify is worse than one
// they have to open, because they cannot tell whether it is the one they
// were waiting for.

// reportHeaderLabel names the block when its goal line is missing —
// nothing else on the row would say what it is.
const reportHeaderLabel = "Delegation report"

// isDelegationReport reports whether this item renders one.
func (m *UserMessageItem) isDelegationReport() bool {
	return !m.queued && message.IsDelegationReport(m.message)
}

// toggleReportExpanded flips a report row between its header and its
// full text. A plain two-state toggle: there is no useful middle window
// into a report, which is one block of prose written to be read whole or
// not at all.
func (m *UserMessageItem) toggleReportExpanded() bool {
	m.reportExpanded = !m.reportExpanded
	m.clearCache()
	m.Bump()
	return m.reportExpanded
}

// renderReportHeader renders the collapsed row: a disclosure triangle,
// what the delegation was asked to do, and how it ended.
//
// The goal is read through the same headline logic the chat's delegation
// block and the session panel use, so the report of a piece of work is
// labeled the way that work was labeled while it ran, rather than by the
// scaffolding line a structured prompt happens to start with.
func (m *UserMessageItem) renderReportHeader(width int) string {
	text := m.message.Content().Text
	subject := DelegationHeadline(
		message.DelegationReportField(text, "name"),
		message.DelegationReportField(text, "goal"),
	)
	if strings.TrimSpace(subject) == "" {
		subject = reportHeaderLabel
	}

	glyph := collapsedGlyph
	if m.reportExpanded {
		glyph = expandedGlyph
	}
	line := fmt.Sprintf("%s %s", glyph, firstLine(subject))
	if status := message.DelegationReportField(text, "status"); status != "" {
		line += " · " + status
	}
	if width > 0 {
		line = ansi.Truncate(line, width, "…")
	}
	rendered := m.sty.Messages.Notice.Render(line)
	if m.reportExpanded {
		return rendered
	}
	// The hint indents under the subject rather than under the glyph, so
	// the triangle stays the leftmost thing on the row and the two lines
	// read as one control.
	hint := "  " + expandHint
	if width > 0 {
		hint = ansi.Truncate(hint, width, "…")
	}
	return rendered + "\n" + m.sty.Messages.Notice.Render(hint)
}

// renderReportFooter renders the line that closes an opened report, the
// same one every other expandable block in the chat ends with.
func (m *UserMessageItem) renderReportFooter(width int) string {
	hint := " " + collapseHint
	if width > 0 {
		hint = ansi.Truncate(hint, width, "…")
	}
	return m.sty.Messages.Notice.Render(hint)
}

// reportClickHeight is how many rows at the top of a report are its
// header, and therefore a click target: two while collapsed (the subject
// and its hint), one once open — the row that closes it again. The
// opened text below is not a target, so a click in the middle of what
// someone is reading cannot close it; the other way out is the footer,
// on the last line.
func (m *UserMessageItem) reportClickHeight() int {
	if m.reportExpanded {
		return 1
	}
	return 2
}

// reportHit reports whether y — counted from the first line of the
// message body, past the turn separator — lands on one of the report's
// two controls.
func (m *UserMessageItem) reportHit(y int) bool {
	body := y - m.headerLineCount()
	if body >= 0 && body < m.reportClickHeight() {
		return true
	}
	return m.reportExpanded && m.reportRenderedLines > 0 && y == m.reportRenderedLines-1
}
