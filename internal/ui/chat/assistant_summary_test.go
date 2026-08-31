package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// summaryText is the body every summary in these tests carries. It is
// distinctive enough that asserting on its absence proves the collapsed
// row really is withholding the summary rather than merely truncating it.
const summaryText = "ZZQUUX the conversation covered the parser rewrite and the migration"

// summaryMessage builds a finished summarize-pass message, optionally
// carrying the token counts a compaction records.
func summaryMessage(id string, before, after int64) *message.Message {
	return &message.Message{
		ID:   id,
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{
				Thinking:   "deciding what to keep",
				StartedAt:  testStartedAt,
				FinishedAt: testFinishedAt,
			},
			message.TextContent{Text: summaryText},
			message.Finish{Reason: message.FinishReasonEndTurn, Time: testFinishTime},
		},
		IsSummaryMessage:    true,
		SummaryBeforeTokens: before,
		SummaryAfterTokens:  after,
	}
}

// TestSummaryRendersCollapsedByDefault is the whole point of the feature:
// a finished summarize pass must not spill its body into the chat. The
// summary text is what compaction produces for the *model*, and it is
// long by construction, so a person who was mid-task gets their window
// taken over by it. Reasoning is checked too — it would otherwise leak
// through the thinking section even with the content hidden.
func TestSummaryRendersCollapsedByDefault(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewAssistantMessageItem(&sty, summaryMessage("s1", 47000, 8000)).(*AssistantMessageItem)

	out := item.Render(80)
	require.NotContains(t, out, "ZZQUUX",
		"a collapsed summary must not render its body")
	require.NotContains(t, out, "deciding what to keep",
		"a collapsed summary must not render its reasoning either")
	require.Contains(t, out, summaryHeaderLabel)
	require.Contains(t, out, summaryCollapsedGlyph)
	require.Contains(t, out, summaryHeaderHint,
		"the collapsed row must name the key that opens it")
}

// TestSummaryTokenCountsRendered checks the numbers reach the row, and
// that they are formatted the same way the rest of the UI formats token
// counts rather than as raw integers.
func TestSummaryTokenCountsRendered(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewAssistantMessageItem(&sty, summaryMessage("s1", 47000, 8000)).(*AssistantMessageItem)

	out := item.Render(80)
	require.Contains(t, out, "47.0k")
	require.Contains(t, out, "8.0k")
}

// TestSummaryWithoutCountsOmitsThem covers every summary written before
// the counts were recorded: retroactive collapsing is the point, so these
// rows must render, and they must not claim a compaction from zero.
func TestSummaryWithoutCountsOmitsThem(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewAssistantMessageItem(&sty, summaryMessage("s1", 0, 0)).(*AssistantMessageItem)

	out := item.Render(80)
	require.Contains(t, out, summaryHeaderLabel,
		"a summary with no recorded counts must still collapse")
	require.NotContains(t, out, "→",
		"absent counts must be omitted, not rendered as a saving of zero")
	require.NotContains(t, out, "ZZQUUX")
}

// TestSummaryExpandRevealsBody drives the same Expandable interface the
// `space` binding dispatches through (model.Chat.ToggleExpandedSelectedItem),
// so this covers the actual keyboard path and not just the method.
func TestSummaryExpandRevealsBody(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewAssistantMessageItem(&sty, summaryMessage("s1", 47000, 8000)).(*AssistantMessageItem)

	exp, ok := any(item).(Expandable)
	require.True(t, ok)

	require.True(t, exp.ToggleExpanded(), "first toggle must report expanded")
	out := item.Render(80)
	require.Contains(t, out, "ZZQUUX", "an expanded summary must show its body")
	require.Contains(t, out, summaryExpandedGlyph,
		"the header must flip to the open disclosure triangle")
	require.NotContains(t, out, summaryHeaderHint,
		"an already-open row must not advertise the key that opens it")

	require.False(t, exp.ToggleExpanded(), "second toggle must report collapsed")
	require.NotContains(t, item.Render(80), "ZZQUUX")
}

// TestSummaryExpandsWithoutThinking guards the reason summaries do not
// share the thinking cycle: that cycle is a no-op when a message carries
// no reasoning, which would leave a summary permanently unopenable.
func TestSummaryExpandsWithoutThinking(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	msg := summaryMessage("s1", 47000, 8000)
	msg.Parts = []message.ContentPart{
		message.TextContent{Text: summaryText},
		message.Finish{Reason: message.FinishReasonEndTurn, Time: testFinishTime},
	}
	item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)

	require.True(t, item.ToggleExpanded())
	require.Contains(t, item.Render(80), "ZZQUUX")
}

// TestFailedSummaryStaysVisible: hiding a summarize that failed would be
// indistinguishable from one that worked, while the context it was meant
// to compact is still sitting there uncompacted.
func TestFailedSummaryStaysVisible(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	msg := summaryMessage("s1", 0, 0)
	msg.Parts = []message.ContentPart{
		message.Finish{
			Reason:  message.FinishReasonError,
			Message: "Summarization Error",
			Details: "provider refused",
			Time:    testFinishTime,
		},
	}
	item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)

	out := item.Render(80)
	require.Contains(t, out, summaryHeaderLabel,
		"a failed summarize keeps its row so it reads as a compaction")
	require.Contains(t, out, "Summarization Error",
		"a failed summarize must keep its error banner while collapsed")
}

// TestNonSummaryAssistantUnaffected pins the blast radius: the collapsed
// row is reached only through IsSummaryMessage, and an ordinary reply
// must keep rendering its body and keep the thinking-cycle toggle.
func TestNonSummaryAssistantUnaffected(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	msg := summaryMessage("m1", 0, 0)
	msg.IsSummaryMessage = false
	item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)

	out := item.Render(80)
	require.Contains(t, out, "ZZQUUX")
	require.NotContains(t, out, summaryHeaderLabel)

	require.True(t, item.ToggleExpanded())
	require.Equal(t, thinkingFullExpanded, item.thinkingViewMode,
		"an ordinary assistant message must still use the thinking cycle")
}

// TestSummarySavingsKnown covers the predicate the row depends on: a
// message that is not a summary, or one with no recorded "before", has
// no numbers to show.
func TestSummarySavingsKnown(t *testing.T) {
	t.Parallel()

	msg := summaryMessage("s1", 47000, 8000)
	before, after, ok := msg.SummarySavings()
	require.True(t, ok)
	require.Equal(t, int64(47000), before)
	require.Equal(t, int64(8000), after)

	msg.IsSummaryMessage = false
	_, _, ok = msg.SummarySavings()
	require.False(t, ok, "a non-summary message reports no savings")

	plain := summaryMessage("s2", 0, 0)
	_, _, ok = plain.SummarySavings()
	require.False(t, ok, "a summary with no recorded before-count reports none")
}

// TestSummaryStreamingShowsOnlySpinner is the "hide it from the start"
// half of the contract: while the pass is running, the partial summary
// must not stream into the window even though it is arriving.
func TestSummaryStreamingShowsOnlySpinner(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	msg := &message.Message{
		ID:   "s1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "half a thought", StartedAt: testStartedAt},
		},
		IsSummaryMessage: true,
	}
	item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)

	require.False(t, msg.IsFinished(), "precondition: the pass is still running")
	out := item.Render(80)
	require.NotContains(t, out, "half a thought",
		"a running summarize must not stream its reasoning into the chat")
	require.False(t, strings.Contains(out, summaryText))
}

// TestSummaryHintIsItsOwnLine pins the two-line shape. The hint used to
// trail the counts in parentheses, where it read as a footnote to the
// numbers and was the first thing a narrow window truncated away — and
// it is the only thing on the row saying the row opens at all.
func TestSummaryHintIsItsOwnLine(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewAssistantMessageItem(&sty, summaryMessage("s1", 47000, 8000)).(*AssistantMessageItem)

	lines := strings.Split(item.Render(80), "\n")
	require.Len(t, lines, 2)
	require.Contains(t, lines[0], summaryHeaderLabel)
	require.NotContains(t, lines[0], summaryHeaderHint,
		"the hint must not ride along on the line the counts are truncated from")
	require.Contains(t, lines[1], summaryHeaderHint)
}

// TestSummaryOpensOnAClick is the fix for the complaint that started
// this: the row said "space to expand" and the pointer did nothing, so
// the affordance people actually reach for first was the one that was
// not wired up.
func TestSummaryOpensOnAClick(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewAssistantMessageItem(&sty, summaryMessage("s1", 47000, 8000)).(*AssistantMessageItem)

	for _, y := range []int{0, 1} {
		require.True(t, item.HandleMouseClick(ansi.MouseLeft, 0, y),
			"both rows of a collapsed summary must be clickable (row %d)", y)
		require.True(t, item.HoverableAt(0, y, 80),
			"and must highlight where they can be clicked (row %d)", y)
	}
	require.False(t, item.HandleMouseClick(ansi.MouseLeft, 0, 2),
		"nothing below the header belongs to the control")

	require.True(t, item.ToggleExpanded())
	out := item.Render(80)
	require.Contains(t, out, "ZZQUUX", "the click must open the body")

	// Open, the header is one row: the one that closes it again. A click
	// in the text below must not close what the person is reading.
	require.True(t, item.HandleMouseClick(ansi.MouseLeft, 0, 0))
	require.False(t, item.HandleMouseClick(ansi.MouseLeft, 0, 1))
}
