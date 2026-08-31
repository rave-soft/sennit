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

	summaryHeaderLabel = "Context compacted"
	summaryHeaderHint  = "space to expand"
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
	text := fmt.Sprintf("%s %s", glyph, summaryHeaderLabel)
	if before, after, ok := a.message.SummarySavings(); ok {
		text += fmt.Sprintf(" · %s → %s",
			presentation.FormatTokenCount(before),
			presentation.FormatTokenCount(after),
		)
	}
	if !a.summaryExpanded {
		text += "  (" + summaryHeaderHint + ")"
	}
	if width > 0 {
		text = ansi.Truncate(text, width, "…")
	}
	return a.sty.Messages.Notice.Render(text)
}

// renderCollapsedSummary renders the whole item while the summary is
// collapsed. It deliberately bypasses renderMessageContent: none of the
// summary's own text, reasoning included, reaches the window.
func (a *AssistantMessageItem) renderCollapsedSummary(width int) string {
	// Still running: the working spinner is the entire row. Its label
	// comes from the message's own phase, so it reads "Summarizing"
	// without this needing to know that.
	if a.isSpinning() {
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
