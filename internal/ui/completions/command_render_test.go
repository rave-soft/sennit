package completions

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/sahilm/fuzzy"
	"github.com/stretchr/testify/require"
)

// TestCommandItemRender_ShowsDescriptionInParens covers the requested popup
// format: "title (description)", with the description a separate, muted
// segment after the (possibly match-highlighted) title.
func TestCommandItemRender_ShowsDescriptionInParens(t *testing.T) {
	t.Parallel()

	item := NewCommandCompletionItem(
		CommandCompletionValue{Title: "compact", Description: "summarize the session"},
		lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(),
	)

	rendered := item.Render(60)
	require.Equal(t, "compact (summarize the session)", strings.TrimSpace(ansi.Strip(rendered)))
}

// TestCommandItemRender_NoDescriptionOmitsParens covers commands with no
// description (e.g. plain custom commands without frontmatter metadata):
// no trailing "()" should appear.
func TestCommandItemRender_NoDescriptionOmitsParens(t *testing.T) {
	t.Parallel()

	item := NewCommandCompletionItem(
		CommandCompletionValue{Title: "my-command"},
		lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(),
	)

	rendered := item.Render(60)
	require.Equal(t, "my-command", strings.TrimSpace(ansi.Strip(rendered)))
}

// TestCommandItemRender_DescriptionIsMutedSeparateFromMatch covers the
// "not part of the match text" requirement: highlighting a match inside the
// title must not touch the "(description)" suffix, and the suffix must
// render in the muted style rather than the match style.
func TestCommandItemRender_DescriptionIsMutedSeparateFromMatch(t *testing.T) {
	t.Parallel()

	matchStyle := lipgloss.NewStyle().Bold(true)
	mutedStyle := lipgloss.NewStyle().Italic(true)

	item := NewCommandCompletionItem(
		CommandCompletionValue{Title: "compact", Description: "summarize the session"},
		lipgloss.NewStyle(), lipgloss.NewStyle(), matchStyle, mutedStyle,
	)

	// Match the first three bytes of the title ("com"), as fuzzy.Find would
	// for a query typed against Filter() (title+aliases+description) that
	// happens to match early in the title.
	item.SetMatch(fuzzy.Match{MatchedIndexes: []int{0, 1, 2}})

	rendered := item.Render(60)

	// The suffix is rendered as a standalone muted segment.
	require.Contains(t, rendered, mutedStyle.Render(" (summarize the session)"),
		"description suffix must be its own muted-styled segment")

	// The matched title prefix keeps its own highlight, styled
	// independently of (and not overlapping) the muted suffix.
	require.Contains(t, rendered, matchStyle.Render("com"),
		"matched title prefix must keep the match style")
}

// TestCommandItemRender_MatchPastTitleIsClipped is a robustness check: when
// match.MatchedIndexes reach past the title (as they would for a query that
// matched an alias or the description rather than the title, since
// Filter() includes both), rendering must not panic and must still produce
// the expected "title (description)" text — the match segment is simply
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
		require.Equal(t, "new (start a new session)", strings.TrimSpace(ansi.Strip(rendered)))
	})
}

// TestFitDescriptionSuffix_TruncatesDescriptionNotTitle covers width
// budgeting: the description is truncated (or dropped) to fit, while the
// title is never touched by this function.
func TestFitDescriptionSuffix_TruncatesDescriptionNotTitle(t *testing.T) {
	t.Parallel()

	t.Run("fits fully", func(t *testing.T) {
		t.Parallel()
		got := fitDescriptionSuffix("short", 20)
		require.Equal(t, " (short)", got)
	})

	t.Run("truncates a long description", func(t *testing.T) {
		t.Parallel()
		got := fitDescriptionSuffix("a rather long description text", 12)
		require.LessOrEqual(t, ansi.StringWidth(got), 12)
		require.True(t, strings.HasPrefix(got, " ("))
		require.Contains(t, got, "…")
	})

	t.Run("drops entirely when there's no room", func(t *testing.T) {
		t.Parallel()
		got := fitDescriptionSuffix("summarize the session", 2)
		require.Empty(t, got)
	})

	t.Run("empty description yields no suffix", func(t *testing.T) {
		t.Parallel()
		require.Empty(t, fitDescriptionSuffix("", 20))
	})
}

// TestCommandItemRender_TruncatesDescriptionAtNarrowWidth is the
// render-level counterpart: at a width too narrow for the full
// "title (description)", the title stays intact and the description is
// what gives way (truncated with an ellipsis, or dropped).
func TestCommandItemRender_TruncatesDescriptionAtNarrowWidth(t *testing.T) {
	t.Parallel()

	item := NewCommandCompletionItem(
		CommandCompletionValue{Title: "compact", Description: "summarize the session"},
		lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(),
	)

	rendered := ansi.Strip(item.Render(16))
	plain := strings.TrimSpace(rendered)
	require.True(t, strings.HasPrefix(plain, "compact"), "title must survive truncation intact, got %q", plain)
	require.LessOrEqual(t, ansi.StringWidth(plain), 14)
}
