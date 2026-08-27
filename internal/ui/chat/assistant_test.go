package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestAssistantMessageItemExpandable guards the Expandable contract on
// AssistantMessageItem along the keyboard-driven expand path. The earlier
// implementation returned no value, which meant the type silently did
// not satisfy chat.Expandable and the keyboard-driven expand path in
// model/chat.go skipped thinking blocks.
//
// We exercise the contract through the bare Expandable interface (the
// same dispatch site model.Chat.ToggleExpandedSelectedItem uses), which
// proves both that AssistantMessageItem still satisfies the interface
// and that the bool return reports the right semantic state at every
// point in the cycle.
func TestAssistantMessageItemExpandable(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	// Short thinking: under the tail-window cap, so the cycle is
	// collapsed -> full -> collapsed (tail-window is skipped).
	msg := thinkingMessage("m1", "step one\nstep two\nstep three", "")
	item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)

	exp, ok := any(item).(Expandable)
	require.True(t, ok, "AssistantMessageItem must satisfy Expandable")

	require.Equal(t, thinkingCollapsed, item.thinkingViewMode,
		"new items must start in the collapsed view-mode")
	require.True(t, exp.ToggleExpanded(),
		"first toggle of a non-empty thinking block must report expanded")
	require.Equal(t, thinkingFullExpanded, item.thinkingViewMode,
		"short blocks must skip tail-window and land in full expansion")
	require.False(t, exp.ToggleExpanded(),
		"second toggle must report collapsed (cycle closed)")
	require.Equal(t, thinkingCollapsed, item.thinkingViewMode)
}

// TestAssistantMessageItemExpandableEmptyThinkingNoOp guards the B2
// fix: a message with no thinking text must treat ToggleExpanded as a
// no-op. Mutating the view mode in that case would thrash the
// thinking-section cache key for no visible benefit and would surprise
// the caller (model.Chat.ToggleExpandedSelectedItem would treat a
// "now collapsed" return as a real state change and re-scroll on it).
func TestAssistantMessageItemExpandableEmptyThinkingNoOp(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	msg := &message.Message{ID: "m1-empty", Role: message.Assistant}
	item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)

	exp, ok := any(item).(Expandable)
	require.True(t, ok, "AssistantMessageItem must satisfy Expandable")

	require.Equal(t, thinkingCollapsed, item.thinkingViewMode)
	require.False(t, exp.ToggleExpanded(),
		"empty thinking must report current (collapsed) state without flipping")
	require.Equal(t, thinkingCollapsed, item.thinkingViewMode,
		"empty-thinking toggle must not mutate thinkingViewMode")

	// Whitespace-only thinking is still effectively empty.
	item.message.Parts = []message.ContentPart{
		message.ReasoningContent{Thinking: "  \n\n\t  ", StartedAt: testStartedAt},
	}
	require.False(t, exp.ToggleExpanded())
	require.Equal(t, thinkingCollapsed, item.thinkingViewMode)
}

// TestAssistantMessageItemTailWindowBoundary guards the B1 fix: the
// tail-window heuristic must compare logical line counts (1 +
// newline count) against the cap, not raw newline counts. A source
// whose logical line count exactly equals the cap must NOT trip the
// tail-window step (full render still fits cleanly under the cap),
// while one logical line over the cap must trip it.
func TestAssistantMessageItemTailWindowBoundary(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()

	atCap := buildLines(maxExpandedThinkingTailLines)
	overCap := buildLines(maxExpandedThinkingTailLines + 1)

	atItem := NewAssistantMessageItem(&sty, thinkingMessage("at-cap", atCap, "")).(*AssistantMessageItem)
	require.False(t, atItem.tailWindowWouldTruncate(),
		"a source with exactly N logical lines must not trip the tail-window step")

	overItem := NewAssistantMessageItem(&sty, thinkingMessage("over-cap", overCap, "")).(*AssistantMessageItem)
	require.True(t, overItem.tailWindowWouldTruncate(),
		"a source with N+1 logical lines must trip the tail-window step")
}

// buildLines returns a string of n logical lines (n-1 newlines). Each
// line is a unique short token so callers can distinguish head from
// tail in rendered output if they need to.
func buildLines(n int) string {
	if n <= 0 {
		return ""
	}
	var b []byte
	for i := 1; i <= n; i++ {
		if i > 1 {
			b = append(b, '\n')
		}
		b = append(b, 'l', 'n')
		b = append(b, []byte(itoa(i))...)
	}
	return string(b)
}

// TestAssistantMessageItemHandleMouseClick ensures HandleMouseClick does not
// toggle expansion on its own. The generic Expandable path in
// model/chat.go does the toggle; doing it here too would double-toggle and
// net to no change.
func TestAssistantMessageItemHandleMouseClick(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	msg := &message.Message{ID: "m2", Role: message.Assistant}
	item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)
	item.thinkingBoxHeight = 5

	// Click inside the thinking box signals handled but must not mutate
	// the view-mode state.
	require.True(t, item.HandleMouseClick(ansi.MouseLeft, 0, 2))
	require.Equal(t, thinkingCollapsed, item.thinkingViewMode,
		"HandleMouseClick must not toggle expansion on its own")

	// Click outside the thinking box is ignored entirely.
	require.False(t, item.HandleMouseClick(ansi.MouseLeft, 0, 10))
	require.Equal(t, thinkingCollapsed, item.thinkingViewMode)

	// Non-left button is ignored.
	require.False(t, item.HandleMouseClick(ansi.MouseRight, 0, 2))
	require.Equal(t, thinkingCollapsed, item.thinkingViewMode)
}

func TestAssistantMessageItemHoverMatchesClickTarget(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewAssistantMessageItem(&sty, &message.Message{ID: "m3", Role: message.Assistant}).(*AssistantMessageItem)
	item.thinkingBoxHeight = 3

	require.True(t, item.HoverableAt(0, 0, 80))
	require.True(t, item.HoverableAt(0, 2, 80))
	require.False(t, item.HoverableAt(0, 3, 80))

	version := item.Version()
	item.SetHovered(true)
	require.True(t, item.hovered)
	require.Greater(t, item.Version(), version)
}

// TestAssistantMessageItem_ThinkingHashIncrementalPathSurvivesRender is
// the regression test for prefixCacheKey and cachedThinking each calling
// thinkingKey() independently: thinkingHashIncremental's fast path only
// engages when len(thinking) is strictly greater than the length it
// last saw (see thinkingHashIncremental's comment on why the check must
// be strict). Calling it twice within the same Render pass — once for
// the prefix cache-key check, once again inside the actual render —
// used to mean the second call always saw an unchanged length and fell
// through to a full re-hash, on every single tick, regardless of the
// "incremental" bookkeeping. Render is now built around
// computeSectionKeys so each pass computes every section's key exactly
// once; simulating a run of streaming ticks (each strictly extending the
// previous thinking text) must therefore only ever take the full re-hash
// branch on the very first tick.
func TestAssistantMessageItem_ThinkingHashIncrementalPathSurvivesRender(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	const width = 60

	// Kept comfortably over thinkingHashIncremental's 64-byte sample cap
	// throughout every tick, including the first: below that cap
	// sampleLen tracks the current (growing) text length rather than a
	// pinned 64 bytes, so the divergence-sample comparison never lines
	// up between ticks regardless of this bug — a separate, harmless
	// quirk confined to a message's first 64 bytes of thinking text. That
	// is not what this test is guarding; keeping every tick past the cap
	// isolates the double-call regression on its own.
	full := strings.Repeat("Let me think about this problem carefully. ", 5)
	item := NewAssistantMessageItem(&sty, thinkingMessage("m-stream", full[:80], "")).(*AssistantMessageItem)

	item.Render(width)
	require.Equal(t, 1, item.thinkingHashFullRehashes, "the first tick always seeds via a full hash")

	// Each subsequent tick strictly extends the previous thinking text,
	// the same way real streaming deltas append-only grow it.
	for n := 90; n <= len(full); n += 10 {
		item.SetMessage(thinkingMessage("m-stream", full[:n], ""))
		item.Render(width)
	}
	require.Equal(t, 1, item.thinkingHashFullRehashes,
		"every tick after the first must take the incremental fast path, not re-hash the whole text")
}
