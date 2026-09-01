package chat

import (
	"fmt"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/presentation"
)

// A summary message is the one assistant message nobody asked to read. It
// is written by the summarize pass to replace the conversation behind it,
// it is addressed to the next model rather than to the person watching,
// and it is long by construction — so rendering it like an ordinary reply
// buries the actual conversation under a wall of text at exactly the
// moment the person was in the middle of something.
//
// It is therefore collapsed to a single row, expandable in place through
// the same [Expandable] path (and the same `space` binding) every other
// collapsible item in the chat uses. Collapsed is the state it is born
// in, so the text never streams into the window at all: while the pass is
// running the row is the ordinary working spinner, whose
// [message.PhaseSummarizing] label already says what is happening.
//
// The one thing that is never hidden is a failure: a summarize that ended
// in an error keeps rendering its error banner, collapsed or not, because
// a silently swallowed compaction failure looks identical to a successful
// one while leaving the context uncompacted.

const (
	// summaryCollapsedGlyph and summaryExpandedGlyph are the disclosure
	// triangles the session panel's collapsible sections already use
	// (see panelSectionHeaderText), so a collapsed summary reads as the
	// same affordance rather than a new one.
	summaryCollapsedGlyph = "▸"
	summaryExpandedGlyph  = "▾"

	// summaryHeaderLabel is the label of a finished compaction;
	// summaryRunningLabel is the same row while the pass is still
	// running. A summarize streams its text like any other reply, so the
	// generic spinner test (isSpinning, which requires no content yet)
	// goes false the moment the first delta lands - and the row then
	// showed the finished wording, and its savings counts, for a
	// compaction that had not happened.
	summaryHeaderLabel  = "Context compacted"
	summaryRunningLabel = "Compacting context"
	// summaryHeaderHint and summaryCollapseHint sit on lines of their own
	// rather than in parentheses after the counts. Trailing on the header
	// line the hint read as a footnote to the numbers and got truncated
	// first on a narrow window - and it was the only thing on the row
	// telling anyone the row opened at all.
	//
	// The wording is the one every other expandable block in the chat
	// uses (see expandableBodyContent), and it names clicking alone: the
	// `space` binding works only while the chat list itself holds focus,
	// which is not where a person reading a reply usually is, so
	// advertising it here promised a key that mostly does nothing.
	summaryHeaderHint   = "Click to expand"
	summaryCollapseHint = "Click to collapse"
)

// isSummary reports whether this item renders a summarize pass's output.
func (a *AssistantMessageItem) isSummary() bool {
	return a.message != nil && a.message.IsSummaryMessage
}

// summaryIsCollapsed reports whether the item must render as the one-line
// summary row rather than as its full content.
func (a *AssistantMessageItem) summaryIsCollapsed() bool {
	return a.isSummary() && !a.summaryExpanded
}

// toggleSummaryExpanded flips a summary row between its collapsed line and
// its full text. Unlike the thinking cycle it is a plain two-state toggle:
// there is no useful middle window into a summary, which is a single prose
// block rather than a trace whose tail is the interesting part.
//
// The whole render changes shape here, not just a windowing slice, so every
// section cache is dropped rather than relying on a key that folds the flag
// in — the collapsed row is not a prefix of the expanded one.
func (a *AssistantMessageItem) toggleSummaryExpanded() bool {
	a.summaryExpanded = !a.summaryExpanded
	a.clearCache()
	a.Bump()
	return a.summaryExpanded
}

// renderSummaryHeader renders the summary's one-line header: a disclosure
// triangle, the label, the token counts when they are known, and — while
// collapsed — the hint naming the key that opens it.
//
// The counts are omitted entirely rather than shown as zero when the
// message predates them or the provider reported no usage; see
// [message.Message.SummarySavings].
func (a *AssistantMessageItem) renderSummaryHeader(width int) string {
	glyph := summaryCollapsedGlyph
	if a.summaryExpanded {
		glyph = summaryExpandedGlyph
	}
	label := summaryHeaderLabel
	if !a.message.IsFinished() {
		label = summaryRunningLabel
	}
	text := fmt.Sprintf("%s %s", glyph, label)
	if before, after, ok := a.message.SummarySavings(); ok {
		text += fmt.Sprintf(" · %s → %s",
			presentation.FormatTokenCount(before),
			presentation.FormatTokenCount(after),
		)
	}
	if width > 0 {
		text = ansi.Truncate(text, width, "…")
	}
	rendered := a.sty.Messages.Notice.Render(text)
	if a.summaryExpanded {
		return rendered
	}
	// The hint indents under the label rather than under the glyph, so
	// the disclosure triangle stays the leftmost thing on the row and
	// the two lines read as one control.
	hint := "  " + summaryHeaderHint
	if width > 0 {
		hint = ansi.Truncate(hint, width, "…")
	}
	return rendered + "\n" + a.sty.Messages.Notice.Render(hint)
}

// summaryClickHeight is how many rows at the *top* of a summary item are
// the header, and therefore a click target. Two while collapsed (the
// label and its hint), one once open - the row that closes it again. The
// opened text below is not a target: a click in the middle of what
// someone is reading must not close it. The other target is the footer,
// which renderSummaryFooter puts on the last line; see summaryFooterLine.
//
// A constant rather than a measurement: renderSummaryHeader is the only
// thing that decides this shape, it is right here, and a click target
// read back from a cached render would go stale exactly when the row
// changed shape.
func (a *AssistantMessageItem) summaryClickHeight() int {
	if a.summaryExpanded {
		return 1
	}
	return 2
}

// renderSummaryFooter renders the line that closes an opened summary. It
// exists because the header alone leaves no way back at the bottom of a
// long compaction: the person has scrolled past the row they opened, and
// every other expandable block in the chat ends with this same line.
func (a *AssistantMessageItem) renderSummaryFooter(width int) string {
	hint := " " + summaryCollapseHint
	if width > 0 {
		hint = ansi.Truncate(hint, width, "…")
	}
	return a.sty.Messages.Notice.Render(hint)
}

// summaryFooterLine is the row index of that footer within the item's
// last render, or -1 when there is none (a collapsed summary, or an item
// that is not one). renderRaw records the height as it renders, the same
// way thinkingBoxHeight is recorded, since the footer's position depends
// on how much text the summary itself came to.
func (a *AssistantMessageItem) summaryFooterLine() int {
	if !a.isSummary() || !a.summaryExpanded || a.summaryRenderedLines <= 0 {
		return -1
	}
	return a.summaryRenderedLines - 1
}

// summaryHit reports whether y lands on one of an expanded summary's two
// controls: the header at the top, or the footer on the last line.
func (a *AssistantMessageItem) summaryHit(y int) bool {
	if y >= 0 && y < a.summaryClickHeight() {
		return true
	}
	return y == a.summaryFooterLine()
}

// renderCollapsedSummary renders the whole item while the summary is
// collapsed. It deliberately bypasses renderMessageContent: none of the
// summary's own text, reasoning included, reaches the window.
func (a *AssistantMessageItem) renderCollapsedSummary(width int) string {
	// Still running: the working spinner is the entire row. Its label
	// comes from the message's own phase, so it reads "Summarizing"
	// without this needing to know that.
	//
	// IsFinished, not the generic isSpinning: a summarize streams its
	// text like any other reply, and isSpinning goes false as soon as
	// there is content - which left a running compaction rendering the
	// finished header, savings counts and all.
	if a.spinnerActive() {
		return a.renderSpinning()
	}
	// A failed summarize stays loud. The header still leads so the row
	// remains recognizable as the compaction it was. Cancellation gets
	// the same footer the ordinary path gives it, for the same reason:
	// a compaction that did not happen must not read as one that did.
	header := a.renderSummaryHeader(width)
	if a.message.IsFinished() {
		switch {
		case a.message.FinishReason() == message.FinishReasonCanceled:
			return header + "\n" + a.sty.Messages.AssistantCanceled.Render("Canceled")
		case a.message.IsErrorLike():
			return header + "\n" + a.renderError(width)
		}
	}
	return header
}
