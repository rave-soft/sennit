package completions

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/sahilm/fuzzy"
	"github.com/stretchr/testify/require"
)

// TestCommandItemRender_ColumnAlignsDescription covers the requested popup
// format: title, then the description in its own right-aligned column
// (no parens) — a separate, muted segment after the (possibly
// match-highlighted) title. titleColumn is what Completions.alignColumns
// would compute and push to every visible item; set here directly to test
// the item in isolation.
func TestCommandItemRender_ColumnAlignsDescription(t *testing.T) {
	t.Parallel()

	item := NewCommandCompletionItem(
		CommandCompletionValue{Title: "compact", Description: "summarize the session"},
		lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(),
	)
	item.SetTitleColumn(10) // e.g. widest visible title ("providers", 8) + 2

	rendered := item.Render(60)
	// "compact" (7) padded to column 10, then the description — no parens.
	require.Equal(t, "compact   summarize the session", strings.TrimSpace(ansi.Strip(rendered)))
}

// TestCommandItemRender_NoColumnShowsTitleOnly covers the case where no
// column alignment is active (titleColumn == 0, its zero value) — the
// state before Completions.alignColumns has run, and permanently for
// @-file items, which never carry a description at all.
func TestCommandItemRender_NoColumnShowsTitleOnly(t *testing.T) {
	t.Parallel()

	t.Run("no description", func(t *testing.T) {
		t.Parallel()
		item := NewCommandCompletionItem(
			CommandCompletionValue{Title: "my-command"},
			lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(),
		)
		rendered := item.Render(60)
		require.Equal(t, "my-command", strings.TrimSpace(ansi.Strip(rendered)))
	})

	t.Run("description present but no column set", func(t *testing.T) {
		t.Parallel()
		item := NewCommandCompletionItem(
			CommandCompletionValue{Title: "compact", Description: "summarize the session"},
			lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(),
		)
		rendered := item.Render(60)
		require.Equal(t, "compact", strings.TrimSpace(ansi.Strip(rendered)))
	})
}

// TestCommandItemRender_DescriptionColumnIsMutedSeparateFromMatch covers
// the "not part of the match text" requirement: highlighting a match
// inside the title must not touch the description column, and the
// description must render in the muted style rather than the match style.
func TestCommandItemRender_DescriptionColumnIsMutedSeparateFromMatch(t *testing.T) {
	t.Parallel()

	matchStyle := lipgloss.NewStyle().Bold(true)
	mutedStyle := lipgloss.NewStyle().Italic(true)

	item := NewCommandCompletionItem(
		CommandCompletionValue{Title: "compact", Description: "summarize the session"},
		lipgloss.NewStyle(), lipgloss.NewStyle(), matchStyle, mutedStyle,
	)
	item.SetTitleColumn(10)

	// Match the first three bytes of the title ("com"), as fuzzy.Find would
	// for a query typed against Filter() (title+aliases+description) that
	// happens to match early in the title.
	item.SetMatch(fuzzy.Match{MatchedIndexes: []int{0, 1, 2}})

	rendered := item.Render(60)

	// The description is rendered as a standalone muted segment — no
	// parens, and no bleed from the match style.
	require.Contains(t, rendered, mutedStyle.Render("summarize the session"),
		"description column must be its own muted-styled segment")

	// The matched title prefix keeps its own highlight, styled
	// independently of (and not overlapping) the description.
	require.Contains(t, rendered, matchStyle.Render("com"),
		"matched title prefix must keep the match style")
}

// TestCommandItemRender_MatchPastTitleIsClipped is a robustness check: when
// match.MatchedIndexes reach past the title (as they would for a query that
// matched an alias or the description rather than the title, since
// Filter() includes both), rendering must not panic and must still produce
// the expected "title  description" text — the match segment is simply
// clipped to the title, per the "match text ends at the title" contract.
func TestCommandItemRender_MatchPastTitleIsClipped(t *testing.T) {
	t.Parallel()

	item := NewCommandCompletionItem(
		CommandCompletionValue{
			Title:       "new",
			Aliases:     []string{"new session", "clear"},
			Description: "start a new session",
		},
		lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle().Bold(true), lipgloss.NewStyle().Italic(true),
	)
	item.SetTitleColumn(6)

	// "clear" only appears in the aliases, well past the title's 3 bytes
	// ("new"), inside Filter() = "new new session clear summarize...".
	filterStr := item.Filter()
	idx := strings.Index(filterStr, "clear")
	require.Greater(t, idx, len(item.Text()))

	indexes := make([]int, len("clear"))
	for i := range indexes {
		indexes[i] = idx + i
	}
	item.SetMatch(fuzzy.Match{MatchedIndexes: indexes})

	require.NotPanics(t, func() {
		rendered := item.Render(60)
		require.Equal(t, "new   start a new session", strings.TrimSpace(ansi.Strip(rendered)))
	})
}

// TestFitColumnDescription covers width budgeting: the description is
// truncated (or dropped) to fit, while the title is never touched by this
// function — only the padding/description it returns.
func TestFitColumnDescription(t *testing.T) {
	t.Parallel()

	t.Run("fits fully", func(t *testing.T) {
		t.Parallel()
		pad, fitted := fitColumnDescription("short", 3, 6, 30)
		require.Equal(t, "   ", pad) // titleColumn(6) - titleWidth(3)
		require.Equal(t, "short", fitted)
	})

	t.Run("truncates a long description", func(t *testing.T) {
		t.Parallel()
		_, fitted := fitColumnDescription("a rather long description text", 3, 6, 12)
		require.LessOrEqual(t, ansi.StringWidth(fitted), 6) // innerWidth(12) - titleColumn(6)
		require.Contains(t, fitted, "…")
	})

	t.Run("drops entirely when there's no room", func(t *testing.T) {
		t.Parallel()
		pad, fitted := fitColumnDescription("summarize the session", 3, 10, 10)
		require.Empty(t, pad)
		require.Empty(t, fitted)
	})

	t.Run("no column alignment active", func(t *testing.T) {
		t.Parallel()
		pad, fitted := fitColumnDescription("summarize the session", 3, 0, 30)
		require.Empty(t, pad)
		require.Empty(t, fitted)
	})

	t.Run("empty description yields nothing", func(t *testing.T) {
		t.Parallel()
		pad, fitted := fitColumnDescription("", 3, 6, 30)
		require.Empty(t, pad)
		require.Empty(t, fitted)
	})
}

// TestCommandItemRender_TruncatesDescriptionAtNarrowWidth is the
// render-level counterpart: at a width too narrow for the full
// "title  description" row, the title stays intact and the description is
// what gives way (truncated with an ellipsis, or dropped).
func TestCommandItemRender_TruncatesDescriptionAtNarrowWidth(t *testing.T) {
	t.Parallel()

	item := NewCommandCompletionItem(
		CommandCompletionValue{Title: "compact", Description: "summarize the session"},
		lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(),
	)
	item.SetTitleColumn(10)

	rendered := ansi.Strip(item.Render(16))
	plain := strings.TrimSpace(rendered)
	require.True(t, strings.HasPrefix(plain, "compact"), "title must survive truncation intact, got %q", plain)
	require.LessOrEqual(t, ansi.StringWidth(plain), 14)
}
